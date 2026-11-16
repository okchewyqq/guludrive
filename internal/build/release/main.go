// Command release builds GuluDrive: the embedded Web bundle, then one Go binary
// per platform. It is the only implementation of the build, so the Makefile and
// CI cannot drift on artifact names, toolchain settings or publish rules.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

const (
	webDir   = "web"
	assets   = "internal/webui/dist"
	distDir  = "dist"
	fontsDir = "assets/fonts"
	uiModule = "ui"
	service  = "./cmd/guludrive" // root module: Web-only service
	desktop  = "./cmd/desktop"   // ui module: Web + native host
)

// Toolchain settings are fixed here so every entry point builds identically.
var goEnv = []string{"GOTOOLCHAIN=go1.27.3", "GOWORK=off", "CGO_ENABLED=0"}

type job struct {
	Name   string // artifact file name, extension included
	Native bool   // needs the ui module and the mygo_noinspector tag
	OS     string // GOOS
	Arch   string // GOARCH
}

func main() {
	oses := flag.String("os", "", "comma separated GOOS list: windows, linux, darwin (default: host)")
	arches := flag.String("arch", "", "comma separated GOARCH list: amd64, arm64 (default: host)")
	nativeOnly := flag.Bool("native", false, "build only the Web + native host binary")
	webOnly := flag.Bool("no-ui", false, "build only the Web-only service binary")
	web := flag.Bool("web", true, "build the embedded Web assets first")
	onlyWeb := flag.Bool("web-only", false, "build only the embedded Web assets")
	check := flag.Bool("check", false, "vet and test both Go modules, check the Worker script")
	parallel := flag.Int("jobs", 0, "maximum concurrent Go builds (0: one per CPU)")
	flag.Usage = func() { fmt.Print(usage) }
	flag.Parse()

	var err error
	switch {
	case *check:
		err = runChecks()
	case *onlyWeb:
		err = buildAssets()
	default:
		err = run(*oses, *arches, *nativeOnly, *webOnly, *web, *parallel)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

const usage = `GuluDrive build - Go 1.27.3, GOWORK=off, CGO_ENABLED=0

  release -web-only            embedded Web assets only (needs Node 22+)
  release                      Web assets, then both binaries for this host
  release -web=false -native   host Web+native binary from the current assets
  release -web=false -no-ui    host Web-only service binary from the current assets
  release -os a,b -arch c,d    one artifact per platform, built concurrently
  release -check               vet and test both modules, run the Worker tests

Native builds need the MiSans faces in assets/fonts: run scripts/fetch-fonts.ps1
or scripts/fetch-fonts.sh once, because the license keeps them out of git.

Artifacts: dist/guludrive[-no-ui]-GOOS-GOARCH[.exe]. Portable builds are Go
only, so build the Web assets first or pass -web=false deliberately.

`

func run(oses, arches string, nativeOnly, webOnly, web bool, parallel int) error {
	if err := os.Chdir(repoRoot()); err != nil {
		return err
	}
	if parallel < 1 {
		parallel = runtime.NumCPU()
	}
	plan, err := planJobs(oses, arches, nativeOnly, webOnly)
	if err != nil {
		return err
	}
	if web {
		if err := buildAssets(); err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(assets, "index.html")); err != nil {
		return fmt.Errorf("missing embedded Web assets in %s; run make web first", assets)
	}
	if err := requireFonts(); err != nil {
		return err
	}

	if err := os.MkdirAll(distDir, 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(distDir, ".build-")
	if err != nil {
		return err
	}
	if stage, err = filepath.Abs(stage); err != nil {
		return err
	}
	keepStage := false
	defer func() {
		if !keepStage {
			os.RemoveAll(stage)
		}
	}()

	var failed []string
	for _, res := range compileAll(plan, stage, parallel) {
		if res.err != nil {
			fmt.Fprintf(os.Stderr, "FAILED %s\n%s\n", res.job.Name, indent(res.err.Error()))
			failed = append(failed, res.job.Name)
			continue
		}
		// A running binary keeps its name: never stop a process to publish.
		if err := os.Rename(res.path, filepath.Join(distDir, res.job.Name)); err != nil {
			keepStage = true
			fmt.Fprintf(os.Stderr, "FAILED %s: %v\n", res.job.Name, err)
			fmt.Fprintf(os.Stderr, "FAILED the verified build was left at %s\n", res.path)
			failed = append(failed, res.job.Name)
			continue
		}
		fmt.Println(filepath.Join(distDir, res.job.Name))
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d artifacts failed: %s", len(failed), len(plan), strings.Join(failed, ", "))
	}
	return nil
}

// repoRoot walks up to the module root so the command runs from any directory.
func repoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "."
		}
		dir = parent
	}
}

func planJobs(oses, arches string, nativeOnly, webOnly bool) ([]job, error) {
	wantOS, err := splitList(oses, []string{"windows", "linux", "darwin"}, runtime.GOOS)
	if err != nil {
		return nil, err
	}
	wantArch, err := splitList(arches, []string{"amd64", "arm64"}, runtime.GOARCH)
	if err != nil {
		return nil, err
	}
	buildNative, buildService := true, true
	if nativeOnly || webOnly {
		buildNative, buildService = nativeOnly, webOnly
	}

	var plan []job
	for _, goos := range wantOS {
		for _, arch := range wantArch {
			ext := ""
			if goos == "windows" {
				ext = ".exe"
			}
			if buildNative {
				plan = append(plan, job{Name: "guludrive-" + goos + "-" + arch + ext, Native: true, OS: goos, Arch: arch})
			}
			if buildService {
				plan = append(plan, job{Name: "guludrive-no-ui-" + goos + "-" + arch + ext, OS: goos, Arch: arch})
			}
		}
	}
	sort.Slice(plan, func(i, j int) bool { return plan[i].Name < plan[j].Name })
	return plan, nil
}

func splitList(value string, allowed []string, fallback string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return []string{fallback}, nil
	}
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" || seen[part] {
			continue
		}
		if !contains(allowed, part) {
			return nil, fmt.Errorf("unsupported value %q; use %s", part, strings.Join(allowed, ", "))
		}
		seen[part] = true
		out = append(out, part)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no target left to build; use %s", strings.Join(allowed, ", "))
	}
	return out, nil
}

// requireFonts explains how to obtain the MiSans faces, which the license keeps
// out of the repository. The native package embeds them with go:embed.
func requireFonts() error {
	for _, weight := range []string{"Regular", "Medium", "Bold"} {
		if _, err := os.Stat(filepath.Join(fontsDir, "MiSans-"+weight+".ttf")); err != nil {
			return fmt.Errorf("missing MiSans fonts in %s; run scripts/fetch-fonts.ps1 or scripts/fetch-fonts.sh once", fontsDir)
		}
	}
	return nil
}

// buildAssets rebuilds the embedded Web bundle; it is the only step needing Node.
func buildAssets() error {
	if err := os.Chdir(repoRoot()); err != nil {
		return err
	}
	if err := runCmd(webDir, nil, "npm", "ci"); err != nil {
		return err
	}
	if err := runCmd(webDir, nil, "npm", "run", "build"); err != nil {
		return err
	}
	fmt.Printf("embedded Web assets: %s\n", assets)
	return nil
}

func runChecks() error {
	if err := os.Chdir(repoRoot()); err != nil {
		return err
	}
	for _, step := range []struct {
		dir  string
		args []string
	}{
		{"", []string{"vet", "./..."}},
		{"", []string{"test", "./..."}},
		{uiModule, []string{"vet", "./..."}},
		{uiModule, []string{"test", "./..."}},
		{"", []string{"--test", "worker/worker.test.mjs"}},
	} {
		name := "go"
		if step.args[0] == "--test" {
			name = "node"
		}
		if err := runStream(step.dir, goEnv, name, step.args...); err != nil {
			return err
		}
	}
	return nil
}

func compileAll(plan []job, stage string, parallel int) []result {
	results := make([]result, len(plan))
	slots := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, j := range plan {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			output := filepath.Join(stage, j.Name)
			results[i] = result{job: j, path: output, err: compile(j, output)}
		}(i, j)
	}
	wg.Wait()
	return results
}

type result struct {
	job  job
	path string
	err  error
}

func compile(j job, output string) error {
	env := append([]string{"GOOS=" + j.OS, "GOARCH=" + j.Arch}, goEnv...)
	args := []string{"build", "-mod=readonly", "-trimpath"}
	dir, pkg, ldflags := "", service, "-s -w"
	if j.Native {
		dir, pkg = uiModule, desktop
		args = append(args, "-tags", "mygo_noinspector")
		if j.OS == "windows" {
			// Hide the console so the Web + native build starts as a GUI app.
			ldflags += " -H=windowsgui"
		}
	}
	args = append(args, "-ldflags", ldflags, "-o", output, pkg)
	return runCmd(dir, env, "go", args...)
}

func runCmd(dir string, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, indent(string(out)))
	}
	return nil
}

// runStream runs a command with its output going straight to the terminal.
func runStream(dir string, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func indent(text string) string {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return "  (no output)"
	}
	return "  " + strings.ReplaceAll(text, "\n", "\n  ")
}
