package store

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"guludrive/core"
	"guludrive/internal/database"
)

// Tiny deterministic carriers keep the segmented VFS smoke tests independent
// of production format thresholds and never contact a real account.
type fakePacker struct{}

func (fakePacker) Plan(size int64, policy core.UploadPolicy) ([]core.PartPlan, error) {
	if size < 0 || !validPolicy(policy) {
		return nil, core.ErrInvalid
	}
	var plans []core.PartPlan
	for offset := int64(0); offset < size || len(plans) == 0; {
		n := min(int64(4), size-offset)
		plans = append(plans, core.PartPlan{Index: len(plans), Offset: offset, Length: n, Kind: "bmp", Budget: 64})
		offset += n
		if offset == size {
			break
		}
	}
	return plans, nil
}
func (fakePacker) PackPart(ctx context.Context, src, dst string, p core.PartPlan) (core.Carrier, error) {
	if err := ctx.Err(); err != nil {
		return core.Carrier{}, err
	}
	f, err := os.Open(src)
	if err != nil {
		return core.Carrier{}, err
	}
	defer f.Close()
	data := make([]byte, p.Length+12)
	binary.LittleEndian.PutUint64(data, uint64(p.Length))
	if _, err = io.ReadFull(io.NewSectionReader(f, p.Offset, p.Length), data[8:8+p.Length]); err != nil {
		return core.Carrier{}, err
	}
	if err = os.WriteFile(dst+".packing", data, 0600); err != nil {
		return core.Carrier{}, err
	}
	if err = os.Rename(dst+".packing", dst); err != nil {
		return core.Carrier{}, err
	}
	c, err := (fakePacker{}).Inspect(dst)
	sum := sha1.Sum(data)
	c.SHA1 = hex.EncodeToString(sum[:])
	return c, err
}
func (fakePacker) Inspect(path string) (core.Carrier, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return core.Carrier{}, err
	}
	if len(b) < 12 {
		return core.Carrier{}, core.ErrInvalid
	}
	n := binary.LittleEndian.Uint64(b)
	if n > uint64(len(b)-12) || n != uint64(len(b)-12) {
		return core.Carrier{}, core.ErrInvalid
	}
	return core.Carrier{Path: path, Format: "test-bmp-v1", Size: int64(len(b)), PayloadOffset: 8, PayloadSize: int64(n)}, nil
}

type fakeAccounts struct{ selected string }

func (a *fakeAccounts) Accounts(ctx context.Context) ([]core.Account, error) {
	return []core.Account{{ID: "account-a", Selected: a.selected != "account-b"}, {ID: "account-b", Selected: a.selected == "account-b"}}, ctx.Err()
}
func (*fakeAccounts) Login(context.Context, string) (core.Account, error) {
	return core.Account{}, core.ErrUnsupported
}
func (a *fakeAccounts) Select(_ context.Context, id string) error { a.selected = id; return nil }
func (*fakeAccounts) Remove(context.Context, string) error        { return core.ErrUnsupported }

type rangeCall struct {
	object         core.RemoteObject
	offset, length int64
}
type albumCall struct {
	album   core.AlbumRef
	objects []core.RemoteObject
}
type fakeBackend struct {
	mu                  sync.Mutex
	data                map[string][]byte
	uploads             []core.RemoteObject
	albums, attachments []albumCall
	opens               []rangeCall
	trashes             []core.RemoteObject
	uploadErr           func(core.UploadRequest) error
	afterUpload         func(core.RemoteObject)
	attachErr           error
	createErr           error
	beforeCreate        func(core.RemoteObject)
	trashErr            map[string]error
	openRange           func(context.Context, core.RemoteObject, int64, int64) (io.ReadCloser, error)
	prepareURL          func(context.Context, core.RemoteObject) (string, error)
}

func (b *fakeBackend) Upload(ctx context.Context, r core.UploadRequest) (core.RemoteObject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return core.RemoteObject{}, err
	}
	if b.uploadErr != nil {
		if err := b.uploadErr(r); err != nil {
			return core.RemoteObject{}, err
		}
	}
	if r.Carrier.SHA1 == "" {
		return core.RemoteObject{}, core.ErrInvalid
	}
	data, err := os.ReadFile(r.Carrier.Path)
	if err != nil {
		return core.RemoteObject{}, err
	}
	o := core.RemoteObject{ID: fmt.Sprintf("remote-%d", len(b.uploads)+1), AccountID: r.Album.AccountID, DedupKey: r.Carrier.SHA1, SHA1: r.Carrier.SHA1, Size: int64(len(data))}
	b.uploads = append(b.uploads, o)
	b.data[o.ID] = data
	if b.afterUpload != nil {
		b.afterUpload(o)
	}
	return o, nil
}
func (b *fakeBackend) CreateAlbum(ctx context.Context, name string, objects []core.RemoteObject) (core.AlbumRef, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(objects) == 0 || len(objects) > 500 {
		return core.AlbumRef{}, core.ErrInvalid
	}
	a := core.AlbumRef{ID: "album-" + name, AccountID: objects[0].AccountID}
	for _, o := range objects {
		if _, ok := b.data[o.ID]; !ok || o.AccountID != a.AccountID {
			return core.AlbumRef{}, core.ErrInvalid
		}
	}
	b.albums = append(b.albums, albumCall{a, append([]core.RemoteObject(nil), objects...)})
	if b.beforeCreate != nil {
		b.beforeCreate(objects[0])
	}
	if b.createErr != nil {
		return core.AlbumRef{}, b.createErr
	}
	return a, ctx.Err()
}
func (b *fakeBackend) AddToAlbum(ctx context.Context, a core.AlbumRef, objects []core.RemoteObject) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(objects) == 0 || len(objects) > 500 {
		return core.ErrInvalid
	}
	for _, o := range objects {
		if o.AccountID != a.AccountID {
			return core.ErrInvalid
		}
	}
	b.attachments = append(b.attachments, albumCall{a, append([]core.RemoteObject(nil), objects...)})
	if b.attachErr != nil {
		return b.attachErr
	}
	return ctx.Err()
}
func (b *fakeBackend) DownloadURL(ctx context.Context, o core.RemoteObject) (string, error) {
	b.mu.Lock()
	prepare := b.prepareURL
	b.mu.Unlock()
	if prepare != nil {
		return prepare(ctx, o)
	}
	return "https://lh3.googleusercontent.com/" + o.ID, ctx.Err()
}
func (b *fakeBackend) OpenRange(ctx context.Context, o core.RemoteObject, offset, length int64) (io.ReadCloser, error) {
	b.mu.Lock()
	b.opens = append(b.opens, rangeCall{o, offset, length})
	fn := b.openRange
	data, ok := b.data[o.ID]
	b.mu.Unlock()
	if fn != nil {
		return fn(ctx, o, offset, length)
	}
	if !ok {
		return nil, core.ErrNotFound
	}
	if offset < 0 || length < 0 || offset > int64(len(data)) || length > int64(len(data))-offset {
		return nil, core.ErrInvalid
	}
	return io.NopCloser(bytes.NewReader(data[offset : offset+length])), ctx.Err()
}
func (b *fakeBackend) Trash(ctx context.Context, o core.RemoteObject) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if strings.HasPrefix(o.ID, "group:") {
		return errors.New("local group reached backend")
	}
	b.trashes = append(b.trashes, o)
	if err := b.trashErr[o.AccountID]; err != nil {
		return err
	}
	for _, r := range b.uploads {
		if r.AccountID == o.AccountID && (r.ID == o.ID || (o.DedupKey != "" && r.DedupKey == o.DedupKey)) {
			delete(b.data, r.ID)
		}
	}
	return ctx.Err()
}
func policy(context.Context) (core.UploadPolicy, error) {
	return core.UploadPolicy{Format: "bmp", PartSize: 150_000_000}, nil
}
func newTestStore(t *testing.T) (*Store, *fakeBackend) {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	b := &fakeBackend{data: map[string][]byte{}, trashErr: map[string]error{}}
	s, err := Open(context.Background(), Config{DB: db, DataDir: dir, Backend: b, Accounts: &fakeAccounts{}, Packer: fakePacker{}, UploadPolicy: policy})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err = s.Mkdir(context.Background(), "/bucket"); err != nil {
		t.Fatal(err)
	}
	return s, b
}
func writer(t *testing.T, s *Store, p, data string) core.WriteSession {
	t.Helper()
	w, err := s.BeginWrite(context.Background(), p, core.WriteOptions{ExpectedLength: int64(len(data))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Abort() })
	if _, err = io.WriteString(w, data); err != nil {
		t.Fatal(err)
	}
	return w
}
func put(t *testing.T, s *Store, p, data string) core.Entry {
	t.Helper()
	e, err := writer(t, s, p, data).Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func get(t *testing.T, s *Store, p string) string {
	t.Helper()
	r, _, err := s.Open(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func objectAt(t *testing.T, s *Store, p string) objectRecord {
	t.Helper()
	n, err := lookup(context.Background(), s.db, p)
	if err != nil {
		t.Fatal(err)
	}
	o, err := s.object(context.Background(), n.object.Int64)
	if err != nil {
		t.Fatal(err)
	}
	return o
}
func reopen(t *testing.T, s *Store) *Store {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := Open(context.Background(), s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { next.Close() })
	return next
}
func emptySpool(t *testing.T, s *Store) {
	t.Helper()
	files, err := os.ReadDir(filepath.Join(s.cfg.DataDir, "temp"))
	if err != nil || len(files) != 0 {
		t.Fatal(files, err)
	}
}

func TestSegmentedReadAndAlbumMembership(t *testing.T) {
	s, b := newTestStore(t)
	ctx := context.Background()
	root, _ := s.Stat(ctx, "/")
	b.mu.Lock()
	b.beforeCreate = func(remote core.RemoteObject) {
		var durable bool
		err := s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM jobs w JOIN objects o ON o.id=w.object JOIN jobs a ON a.object=o.id WHERE w.kind='write' AND w.state='uploaded' AND a.kind='album' AND a.state='creating' AND a.remote=? AND o.remote LIKE 'group:%' AND json_array_length(o.parts,'$.parts')=3 AND NOT EXISTS(SELECT 1 FROM json_each(o.parts,'$.parts') p WHERE json_extract(p.value,'$.uploadState')<>'confirmed'))", remote.ID).Scan(&durable)
		if err != nil || !durable {
			t.Errorf("album RPC preceded durable part receipts: %v", err)
		}
	}
	b.mu.Unlock()
	put(t, s, "/bucket/file", "abcdefghijkl")
	b.mu.Lock()
	b.beforeCreate = nil
	b.mu.Unlock()
	after, _ := s.Stat(ctx, "/")
	if root.ETag() == after.ETag() {
		t.Fatal("ancestor ETag unchanged")
	}
	o := objectAt(t, s, "/bucket/file")
	if len(o.manifest.Parts) != 3 || len(b.albums) != 1 || len(b.albums[0].objects) != 1 || len(b.attachments) != 1 || len(b.attachments[0].objects) != 2 {
		t.Fatal("first album omitted segments")
	}
	var local string
	if err := s.db.QueryRow("SELECT remote FROM objects WHERE id=?", o.id).Scan(&local); err != nil || !strings.HasPrefix(local, "group:") {
		t.Fatal(local, err)
	}
	r, _, err := s.Open(ctx, "/bucket/file")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err = r.Seek(3, io.SeekStart); err != nil || len(b.opens) != 0 {
		t.Fatal("Open/Seek performed I/O", err)
	}
	out, err := io.ReadAll(r)
	if err != nil || string(out) != "defghijkl" || len(b.opens) != 3 || b.opens[0].offset != 11 || b.opens[0].length != 1 || b.opens[1].length != 4 {
		t.Fatal(string(out), b.opens, err)
	}
	if _, err = r.Seek(-2, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	out, err = io.ReadAll(r)
	if err != nil || string(out) != "kl" {
		t.Fatal(string(out), err)
	}
	if _, err = r.Seek(math.MaxInt64, io.SeekEnd); !errors.Is(err, core.ErrInvalid) {
		t.Fatal(err)
	}
	before := len(b.opens)
	source, err := s.PrepareDownload(ctx, "/bucket/file")
	if err != nil || len(source.Segments) != 3 || source.Segments[2].Offset != 8 || source.Segments[1].Size != 4 || len(b.opens) != before {
		t.Fatal(source, err)
	}
	if err = s.Mkdir(ctx, "/copy"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Copy(ctx, "/bucket/file", "/copy/file", false, false); err != nil {
		t.Fatal(err)
	}
	copied := objectAt(t, s, "/copy/file")
	if copied.id != o.id || get(t, s, "/bucket/file") != "abcdefghijkl" {
		t.Fatal("copy must share immutable group")
	}
	for _, album := range []string{"album-bucket", "album-copy"} {
		for _, state := range copied.manifest.AlbumStates[album] {
			if state != "attached" {
				t.Fatal(copied.manifest)
			}
		}
	}
	if len(copied.manifest.AlbumStates) != 2 {
		t.Fatal("cross-album receipts overwrote primary", copied.manifest)
	}
	put(t, s, "/bucket/empty", "")
	before = len(b.opens)
	if get(t, s, "/bucket/empty") != "" || len(b.opens) != before {
		t.Fatal("empty read contacted backend")
	}
	emptySpool(t, s)
	cfg := s.cfg
	cfg.UploadPolicy = nil
	if _, err := Open(ctx, cfg); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("missing policy accepted", err)
	}
	if _, err := s.BeginWrite(ctx, "/bucket/huge", core.WriteOptions{ExpectedLength: math.MaxInt64}); err != nil {
		t.Fatal("unlimited int64 admission", err)
	}
}

func TestCarrierKindMetadata(t *testing.T) {
	s, _ := newTestStore(t)
	const path = "/bucket/kind"
	put(t, s, path, "abcde")
	o := objectAt(t, s, path)
	check := func(want string) {
		entry, err := s.Stat(context.Background(), path)
		if err != nil || entry.CarrierKind != want {
			t.Fatalf("Stat carrier kind %q, want %q: %v", entry.CarrierKind, want, err)
		}
		reader, entry, err := s.Open(context.Background(), path)
		if err != nil || entry.CarrierKind != want {
			t.Fatalf("Open carrier kind %q, want %q: %v", entry.CarrierKind, want, err)
		}
		if err = reader.Close(); err != nil {
			t.Fatal(err)
		}
		source, err := s.PrepareDownload(context.Background(), path)
		if err != nil || source.Entry.CarrierKind != want {
			t.Fatalf("PrepareDownload carrier kind %q, want %q: %v", source.Entry.CarrierKind, want, err)
		}
	}
	check("bmp")
	// Change only plan.kind: neither the logical content type nor policy snapshot
	// determines the HTTP carrier behavior.
	for i := range o.manifest.Parts {
		o.manifest.Parts[i].Plan.Kind = "mp4"
	}
	if err := saveManifest(context.Background(), s.db, o); err != nil {
		t.Fatal(err)
	}
	check("mp4")
	o.manifest.Parts[1].Plan.Kind = "bmp"
	if err := saveManifest(context.Background(), s.db, o); err != nil {
		t.Fatal(err)
	}
	check("")
}

func TestPrepareDownloadBoundedWorkers(t *testing.T) {
	s, b := newTestStore(t)
	put(t, s, "/bucket/prepared", "abcdefghijklmnopqrst") // Five four-byte parts.
	o := objectAt(t, s, "/bucket/prepared")
	indices := map[string]int{}
	for i, p := range o.manifest.Parts {
		indices[p.Remote.ID] = i
	}
	leaseHeld := func() bool { s.stateMu.Lock(); defer s.stateMu.Unlock(); return s.leases[o.id] > 0 }
	type result struct {
		source core.DownloadSource
		err    error
	}
	waitIndex := func(ch <-chan int) int {
		t.Helper()
		select {
		case index := <-ch:
			return index
		case <-time.After(5 * time.Second):
			t.Fatal("URL worker did not progress")
			return -1
		}
	}
	waitResult := func(ch <-chan result) result {
		t.Helper()
		select {
		case r := <-ch:
			return r
		case <-time.After(5 * time.Second):
			t.Fatal("URL preparation did not drain")
			return result{}
		}
	}
	var mu sync.Mutex
	active, maximum := 0, 0
	enter := func() {
		if !leaseHeld() {
			t.Error("group unpinned during URL preparation")
		}
		mu.Lock()
		active++
		maximum = max(maximum, active)
		mu.Unlock()
	}
	leave := func() {
		if !leaseHeld() {
			t.Error("group unpinned before worker exit")
		}
		mu.Lock()
		active--
		mu.Unlock()
	}
	started, finished := make(chan int, 5), make(chan int, 5)
	release := make([]chan struct{}, 5)
	for i := range release {
		release[i] = make(chan struct{})
	}
	defer func() {
		for _, ch := range release {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
	}()
	b.mu.Lock()
	b.prepareURL = func(_ context.Context, r core.RemoteObject) (string, error) {
		index := indices[r.ID]
		enter()
		defer leave()
		started <- index
		<-release[index]
		finished <- index
		return "https://lh3.googleusercontent.com/" + r.ID, nil
	}
	b.mu.Unlock()
	done := make(chan result, 1)
	go func() {
		source, err := s.PrepareDownload(context.Background(), "/bucket/prepared")
		done <- result{source, err}
	}()
	firstFour := make([]int, 4)
	for i := range firstFour {
		firstFour[i] = waitIndex(started)
	}
	mu.Lock()
	peak := maximum
	mu.Unlock()
	if peak != 4 {
		t.Fatal("expected exactly four active URL workers", peak)
	}
	select {
	case index := <-started:
		t.Fatal("unbounded fifth worker", index)
	default:
	}
	// Deliberately finish the initial requests in reverse dispatch order.
	for i := len(firstFour) - 1; i >= 0; i-- {
		close(release[firstFour[i]])
		if got := waitIndex(finished); got != firstFour[i] {
			t.Fatal("unexpected completion", got)
		}
	}
	last := waitIndex(started)
	close(release[last])
	waitIndex(finished)
	prepared := waitResult(done)
	if prepared.err != nil || len(prepared.source.Segments) != 5 {
		t.Fatal(prepared.err)
	}
	for i, segment := range prepared.source.Segments {
		if segment.Offset != int64(i*4) || segment.Size != 4 || segment.URL != "https://lh3.googleusercontent.com/"+o.manifest.Parts[i].Remote.ID {
			t.Fatal("segments lost logical ordering", i)
		}
	}
	mu.Lock()
	count := active
	mu.Unlock()
	if count != 0 || leaseHeld() || len(b.opens) != 0 {
		t.Fatal("preparation leaked workers/pins or read file bytes", count)
	}

	// One safe Google error cancels only sibling work. Hold canceled transports
	// open to prove PrepareDownload waits before returning or releasing its pin.
	started, canceled := make(chan int, 5), make(chan int, 4)
	trigger, drain := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-trigger:
		default:
			close(trigger)
		}
		select {
		case <-drain:
		default:
			close(drain)
		}
	}()
	outer, outerCancel := context.WithCancel(context.Background())
	defer outerCancel()
	b.mu.Lock()
	b.prepareURL = func(ctx context.Context, r core.RemoteObject) (string, error) {
		index := indices[r.ID]
		enter()
		defer leave()
		started <- index
		if index == 0 {
			<-trigger
			return "", &core.RemoteError{Operation: "download", Stage: "http_response", UpstreamStatus: 502, Reason: "rejected", Cause: errors.New("private provider URL")}
		}
		<-ctx.Done()
		canceled <- index
		<-drain
		return "", ctx.Err()
	}
	b.mu.Unlock()
	done = make(chan result, 1)
	go func() { source, err := s.PrepareDownload(outer, "/bucket/prepared"); done <- result{source, err} }()
	for range 4 {
		waitIndex(started)
	}
	close(trigger)
	for range 3 {
		waitIndex(canceled)
	}
	if outer.Err() != nil || !leaseHeld() {
		t.Fatal("child cancellation affected the caller or released its group pin")
	}
	select {
	case <-done:
		t.Fatal("returned before canceled workers drained")
	default:
	}
	select {
	case index := <-started:
		t.Fatal("dispatched work after failure", index)
	default:
	}
	close(drain)
	failed := waitResult(done)
	var diagnostic *core.RemoteError
	if !errors.As(failed.err, &diagnostic) || diagnostic.Operation != "download" || diagnostic.UpstreamStatus != 502 || errors.Is(failed.err, context.Canceled) || strings.Contains(failed.err.Error(), "private") || len(failed.source.Segments) != 0 {
		t.Fatal("first safe diagnostic was replaced or leaked provider detail", failed.err)
	}
	mu.Lock()
	count, peak = active, maximum
	mu.Unlock()
	if count != 0 || peak > 4 || leaseHeld() || outer.Err() != nil {
		t.Fatal("failed preparation did not drain safely", count, peak)
	}
	outerCancel()
	if _, err := s.PrepareDownload(outer, "/bucket/prepared"); !errors.Is(err, context.Canceled) {
		t.Fatal("caller cancellation was not preserved", err)
	}
}

func TestAtomicFailureCancellationAndNoReplay(t *testing.T) {
	s, b := newTestStore(t)
	ctx := context.Background()
	old := put(t, s, "/bucket/file", "old!")
	w := writer(t, s, old.Path, "abcdefghijkl")
	b.mu.Lock()
	b.uploadErr = func(r core.UploadRequest) error {
		if strings.HasSuffix(r.OperationID, ".1") {
			return errors.Join(core.ErrUncertain, errors.New("private provider text"))
		}
		return nil
	}
	b.mu.Unlock()
	if _, err := w.Commit(ctx); !errors.Is(err, core.ErrUncertain) || strings.Contains(err.Error(), "private") {
		t.Fatal(err)
	}
	if get(t, s, old.Path) != "old!" {
		t.Fatal("partial write replaced old file")
	}
	j, err := s.intent(ctx, w.(*writeSession).id)
	if err != nil {
		t.Fatal(err)
	}
	o, err := s.object(ctx, j.object.Int64)
	if err != nil {
		t.Fatal(err)
	}
	if j.state != "uncertain" || o.manifest.Parts[0].UploadState != "confirmed" || o.manifest.Parts[1].UploadState != "uncertain" {
		t.Fatal(j.state, o.manifest)
	}
	b.mu.Lock()
	b.uploadErr = nil
	b.mu.Unlock()
	c, cancel := context.WithCancel(ctx)
	cancelled := writer(t, s, old.Path, "zzzz")
	b.mu.Lock()
	b.afterUpload = func(core.RemoteObject) { cancel() }
	b.mu.Unlock()
	if _, err = cancelled.Commit(c); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	cj, err := s.intent(ctx, cancelled.(*writeSession).id)
	if err != nil {
		t.Fatal(err)
	}
	co, err := s.object(ctx, cj.object.Int64)
	if err != nil || co.manifest.Parts[0].UploadState != "confirmed" {
		t.Fatal("cancel discarded successful receipt", co, err)
	}
	b.mu.Lock()
	b.afterUpload = nil
	count := len(b.uploads)
	b.mu.Unlock()
	if err = s.collect(ctx); err != nil || len(b.trashes) != 0 {
		t.Fatal("unknown upload failed to protect its account", err)
	}
	// Simulate interruption before the unknown-result bookkeeping itself.
	o.manifest.Parts[1].UploadState = "uploading"
	if err = saveManifest(ctx, s.db, o); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE jobs SET state='uploading' WHERE id=?", j.id); err != nil {
		t.Fatal(err)
	}
	s = reopen(t, s)
	if len(b.uploads) != count || get(t, s, old.Path) != "old!" {
		t.Fatal("uncertain upload replayed or canceled write published")
	}
	cas := writer(t, s, old.Path, "CAS!")
	put(t, s, old.Path, "new!")
	if _, err := cas.Commit(ctx); !errors.Is(err, core.ErrConflict) || get(t, s, old.Path) != "new!" {
		t.Fatal("CAS replaced newer file", err)
	}
	emptySpool(t, s)
}

// Seed the exact durable boundary of a server interruption, not a failure that
// has already terminated the write. No large files or live network are used.
func TestRestartResumesConfirmedSnapshot(t *testing.T) {
	s, b := newTestStore(t)
	ctx := context.Background()
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	spool := filepath.Join(s.cfg.DataDir, "temp", id+".raw")
	if err = os.WriteFile(spool, []byte("abcdefghijkl"), 0600); err != nil {
		t.Fatal(err)
	}
	pol, _ := policy(ctx)
	plans, _ := s.cfg.Packer.Plan(12, pol)
	m := core.PartsManifest{Version: 1, LogicalSize: 12, Policy: pol}
	for _, p := range plans {
		m.Parts = append(m.Parts, core.ObjectPart{Plan: p, UploadState: "queued"})
	}
	dst, _ := s.partPath(id, 0)
	carrier, err := s.cfg.Packer.PackPart(ctx, spool, dst, plans[0])
	if err != nil {
		t.Fatal(err)
	}
	remote, err := b.Upload(ctx, core.UploadRequest{Carrier: carrier, Album: core.AlbumRef{AccountID: "account-a"}})
	if err != nil {
		t.Fatal(err)
	}
	m.Parts[0].Carrier, m.Parts[0].Remote, m.Parts[0].UploadState = carrier, remote, "confirmed"
	m.Parts[1].UploadState = "packing"
	dst, _ = s.partPath(id, 1)
	if _, err = s.cfg.Packer.PackPart(ctx, spool, dst, plans[1]); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(dst+".packing", []byte("interrupted"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, _ := manifestJSON(m)
	res, err := s.db.Exec("INSERT INTO objects(account,remote,dedup,sha1,size,format,off,length,unused_at,state,parts) VALUES('account-a',?,'','',12,'parts-v1',0,12,0,'staging',?)", "group:"+id, raw)
	if err != nil {
		t.Fatal(err)
	}
	oid, _ := res.LastInsertId()
	_, err = s.db.Exec("INSERT INTO jobs(id,kind,path,state,created,updated,spool,account,object,length) VALUES(?,'write','/bucket/recovered','uploading',1,1,?,'account-a',?,12)", id, spool, oid)
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.UploadPolicy = func(context.Context) (core.UploadPolicy, error) {
		t.Error("recovery read new settings")
		return core.UploadPolicy{}, core.ErrInvalid
	}
	s = reopen(t, s)
	if len(b.uploads) != 3 || len(b.albums) != 1 || get(t, s, "/bucket/recovered") != "abcdefghijkl" {
		t.Fatal("recovery replayed confirmed part or omitted remainder")
	}
	if o := objectAt(t, s, "/bucket/recovered"); o.manifest.Policy != pol {
		t.Fatal("policy snapshot changed")
	}
	emptySpool(t, s)
}

func TestUnknownAlbumMutationNeverReplays(t *testing.T) {
	for _, create := range []bool{false, true} {
		t.Run(fmt.Sprint("create-", create), func(t *testing.T) {
			s, b := newTestStore(t)
			ctx := context.Background()
			b.mu.Lock()
			if create {
				b.createErr = context.DeadlineExceeded
			} else {
				b.attachErr = core.ErrUncertain
			}
			b.mu.Unlock()
			if _, err := writer(t, s, "/bucket/file", "abcdefghijkl").Commit(ctx); !errors.Is(err, core.ErrUncertain) {
				t.Fatal(err)
			}
			if _, err := s.Stat(ctx, "/bucket/file"); !errors.Is(err, core.ErrNotFound) {
				t.Fatal("unconfirmed album published", err)
			}
			attachments := len(b.attachments)
			s = reopen(t, s)
			if len(b.uploads) != 3 || len(b.albums) != 1 || len(b.attachments) != attachments {
				t.Fatal("unknown album mutation replayed")
			}
			if err := s.collect(ctx); err != nil || len(b.trashes) != 0 {
				t.Fatal("uncertain membership lost protection", err)
			}
		})
	}
	s, b := newTestStore(t)
	ctx := context.Background()
	put(t, s, "/bucket/source", "abcdefghijkl")
	if err := s.Mkdir(ctx, "/uncertain"); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.attachErr = core.ErrUncertain
	b.mu.Unlock()
	if _, err := s.Copy(ctx, "/bucket/source", "/uncertain/file", false, false); !errors.Is(err, core.ErrUncertain) {
		t.Fatal(err)
	}
	before := len(b.attachments)
	s = reopen(t, s)
	if get(t, s, "/bucket/source") != "abcdefghijkl" || len(b.attachments) != before {
		t.Fatal("copy corrupted primary membership or replayed")
	}
	o := objectAt(t, s, "/bucket/source")
	for _, state := range o.manifest.AlbumStates["album-bucket"] {
		if state != "attached" {
			t.Fatal("copy overwrote primary receipts")
		}
	}
	if _, err := s.Stat(ctx, "/uncertain/file"); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("unknown copy published", err)
	}
	if err := s.Remove(ctx, "/bucket/source"); err != nil {
		t.Fatal(err)
	}
	if err := s.collect(ctx); err != nil || len(b.trashes) != 0 {
		t.Fatal("copy receipt did not protect group", err)
	}
}

func TestConfirmedAlbumBindingRecovery(t *testing.T) {
	s, b := newTestStore(t)
	ctx := context.Background()
	if _, err := s.db.Exec("CREATE TEMP TRIGGER fail_album_bind BEFORE UPDATE OF album ON buckets BEGIN SELECT RAISE(FAIL,'private database detail'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := writer(t, s, "/bucket/file", "abcdefghijkl").Commit(ctx); !errors.Is(err, core.ErrUncertain) || strings.Contains(err.Error(), "private") {
		t.Fatal(err)
	}
	var state string
	if err := s.db.QueryRow("SELECT state FROM jobs WHERE kind='write'").Scan(&state); err != nil || state != "uploaded" {
		t.Fatal("confirmed receipt not recoverable", state, err)
	}
	if err := s.collect(ctx); err != nil || len(b.trashes) != 0 {
		t.Fatal("known receipt lost protection", err)
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_album_bind"); err != nil {
		t.Fatal(err)
	}
	s = reopen(t, s)
	if len(b.uploads) != 3 || len(b.albums) != 1 || len(b.attachments) != 1 || len(b.attachments[0].objects) != 2 || get(t, s, "/bucket/file") != "abcdefghijkl" {
		t.Fatal("confirmed create replayed or remainder omitted")
	}
	emptySpool(t, s)
}

func TestPhysicalDedupReadLeasesAndFairGC(t *testing.T) {
	s, b := newTestStore(t)
	ctx := context.Background()
	put(t, s, "/bucket/one", "abcdabcd")
	put(t, s, "/bucket/two", "abcdabcd")
	if _, err := s.Copy(ctx, "/bucket/one", "/bucket/copy", false, false); err != nil {
		t.Fatal(err)
	}
	r, _, err := s.Open(ctx, "/bucket/one")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, p := range []string{"/bucket/one", "/bucket/two"} {
		if err = s.Remove(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.collect(ctx); err != nil || len(b.trashes) != 0 {
		t.Fatal("trashed shared reference", err)
	}
	if err = s.Remove(ctx, "/bucket/copy"); err != nil {
		t.Fatal(err)
	}
	if err = s.collect(ctx); err != nil || len(b.trashes) != 0 {
		t.Fatal("trashed read lease", err)
	}
	data, err := io.ReadAll(r)
	if err != nil || string(data) != "abcdabcd" {
		t.Fatal(string(data), err)
	}
	if err = s.collect(ctx); err != nil || len(b.trashes) != 0 {
		t.Fatal("EOF released lease", err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	if err = s.collect(ctx); err != nil || len(b.trashes) != 1 || len(b.data) != 0 {
		t.Fatal("physical domain not reclaimed exactly once", b.trashes, err)
	}
	var count int
	if err = s.db.QueryRow("SELECT count(*) FROM objects").Scan(&count); err != nil || count != 0 {
		t.Fatal("reclaimed groups retained", count, err)
	}
	put(t, s, "/bucket/fail", "111")
	s.cfg.Accounts.Select(ctx, "account-b")
	if err = s.Mkdir(ctx, "/other"); err != nil {
		t.Fatal(err)
	}
	put(t, s, "/other/pass", "222")
	for _, p := range []string{"/bucket/fail", "/other/pass"} {
		if err = s.Remove(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	// NULL album uncertainty is not account-wide upload uncertainty.
	_, err = s.db.Exec("INSERT INTO jobs(id,kind,path,state,created,updated,account) VALUES('empty-album','album','','uncertain',1,1,'account-b')")
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.trashErr["account-a"] = errors.New("known rejection")
	b.mu.Unlock()
	if err = s.collect(ctx); err == nil {
		t.Fatal("expected first domain rejection")
	}
	if err = s.db.QueryRow("SELECT count(*) FROM objects WHERE account='account-b'").Scan(&count); err != nil || count != 0 {
		t.Fatal("failed account starved another domain", count, err)
	}
}

// Model optional provider metadata on independently published groups without
// creating invalid carriers or relying on the store's domain matcher in fake GC.
func gcAliases(t *testing.T, s *Store, b *fakeBackend, targets []int, keys []string) []objectRecord {
	t.Helper()
	var objects []objectRecord
	for i := range targets {
		p := fmt.Sprintf("/bucket/alias-%d", i)
		put(t, s, p, "same")
		objects = append(objects, objectAt(t, s, p))
	}
	remotes := make([]core.RemoteObject, len(objects))
	for i, target := range targets {
		remotes[i] = objects[target].manifest.Parts[0].Remote
		remotes[i].DedupKey = keys[i]
	}
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	for i := range objects {
		objects[i].manifest.Parts[0].Remote = remotes[i]
		if err := saveManifest(context.Background(), s.db, objects[i]); err != nil {
			t.Fatal(err)
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	data := map[string][]byte{}
	for _, r := range remotes {
		data[r.ID] = b.data[r.ID]
	}
	b.data, b.uploads = data, remotes
	return objects
}

func TestGCMediaIDAliases(t *testing.T) {
	ctx := context.Background()
	for _, keys := range [][]string{{"key", ""}, {"", "key"}} {
		t.Run(fmt.Sprintf("optional-key-%q", keys[0]), func(t *testing.T) {
			s, b := newTestStore(t)
			objects := gcAliases(t, s, b, []int{0, 0}, keys)
			if err := s.Remove(ctx, "/bucket/alias-0"); err != nil {
				t.Fatal(err)
			}
			if err := s.collect(ctx); err != nil || len(b.trashes) != 0 {
				t.Fatal("same-ID reference ignored optional key", b.trashes, err)
			}
			r, _, err := s.Open(ctx, "/bucket/alias-1")
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if err = s.Remove(ctx, "/bucket/alias-1"); err != nil {
				t.Fatal(err)
			}
			if err = s.collect(ctx); err != nil || len(b.trashes) != 0 {
				t.Fatal("same-ID read lease ignored", err)
			}
			if err = r.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err = s.db.Exec("INSERT INTO jobs(id,kind,path,state,created,updated,account,object) VALUES('alias-receipt','album','','uncertain',1,1,'account-a',?)", objects[1].id); err != nil {
				t.Fatal(err)
			}
			if err = s.collect(ctx); err != nil || len(b.trashes) != 0 {
				t.Fatal("same-ID job protection lost", err)
			}
			if _, err = s.db.Exec("UPDATE jobs SET state='published' WHERE id='alias-receipt'"); err != nil {
				t.Fatal(err)
			}
			if err = s.collect(ctx); err != nil || len(b.trashes) != 1 {
				t.Fatal("unlinked aliases not reclaimed once", b.trashes, err)
			}
			if b.trashes[0].ID != objects[0].manifest.Parts[0].Remote.ID || b.trashes[0].DedupKey != "key" {
				t.Fatal("wrong physical delete descriptor", b.trashes)
			}
			if err = s.collect(ctx); err != nil || len(b.trashes) != 1 {
				t.Fatal("alias receipt replayed", err)
			}
		})
	}
	for _, fixture := range []struct {
		name    string
		targets []int
		keys    []string
	}{
		{"same-ID-conflicting-keys", []int{0, 0}, []string{"key-a", "key-b"}},
		{"dedup-linked-ID-conflicting-keys", []int{0, 1, 1}, []string{"key-a", "key-a", "key-b"}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			s, b := newTestStore(t)
			objects := gcAliases(t, s, b, fixture.targets, fixture.keys)
			for i := 0; i < len(objects)-1; i++ {
				if err := s.Remove(ctx, fmt.Sprintf("/bucket/alias-%d", i)); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.collect(ctx); err != nil && !errors.Is(err, core.ErrConflict) {
				t.Fatal(err)
			}
			if len(b.trashes) != 0 {
				t.Fatal("conflicting alias reference was deleted")
			}
			if err := s.Remove(ctx, fmt.Sprintf("/bucket/alias-%d", len(objects)-1)); err != nil {
				t.Fatal(err)
			}
			if err := s.collect(ctx); !errors.Is(err, core.ErrConflict) || len(b.trashes) != 0 {
				t.Fatal("conflicting keys were treated as one confirmed deletion", err)
			}
			for _, before := range objects {
				o, err := s.object(ctx, before.id)
				if err != nil || o.manifest.Parts[0].TrashState != "" {
					t.Fatal("conflicting receipt falsely reclaimed", o, err)
				}
			}
		})
	}
	t.Run("dedup-linked-ID-with-keyless-alias", func(t *testing.T) {
		s, b := newTestStore(t)
		objects := gcAliases(t, s, b, []int{0, 0, 2, 2}, []string{"", "key", "key", ""})
		for i := 0; i < 3; i++ {
			if err := s.Remove(ctx, fmt.Sprintf("/bucket/alias-%d", i)); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.collect(ctx); err != nil || len(b.trashes) != 0 {
			t.Fatal("keyless alias of dedup-linked ID lost its reference", err)
		}
		r, _, err := s.Open(ctx, "/bucket/alias-3")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		if err = s.Remove(ctx, "/bucket/alias-3"); err != nil {
			t.Fatal(err)
		}
		if err = s.collect(ctx); err != nil || len(b.trashes) != 0 {
			t.Fatal("dedup-linked alias read lease lost", err)
		}
		if err = r.Close(); err != nil {
			t.Fatal(err)
		}
		// Inspect durable per-part receipts before collect prunes fully reclaimed groups.
		func() {
			s.mutationMu.Lock()
			defer s.mutationMu.Unlock()
			claim, err := s.claimDeletion(ctx, domainOf(objects[0].manifest.Parts[0].Remote))
			if err != nil || claim == nil {
				t.Fatal(claim, err)
			}
			if claim.remote.ID != objects[0].manifest.Parts[0].Remote.ID || claim.remote.DedupKey != "key" {
				t.Fatal("dispatch chose an arbitrary keyless alias", claim.remote)
			}
			if err = b.Trash(ctx, claim.remote); err != nil {
				t.Fatal(err)
			}
			if err = s.finishDeletion(claim, nil); err != nil {
				t.Fatal(err)
			}
			var id, key, state string
			if err = s.db.QueryRow("SELECT remote,remote_dedup,state FROM jobs WHERE id=?", claim.job).Scan(&id, &key, &state); err != nil || id != claim.remote.ID || key != "key" || state != "published" {
				t.Fatal("physical receipt disagrees with dispatch", id, key, state, err)
			}
			for _, before := range objects {
				o, err := s.object(ctx, before.id)
				if err != nil || o.manifest.Parts[0].TrashState != "trashed" {
					t.Fatal("SQL and Go receipt matching disagree", o, err)
				}
			}
		}()
		if err = s.collect(ctx); err != nil || len(b.trashes) != 1 || len(b.data) != 0 {
			t.Fatal("known aliases were not deleted exactly once", b.trashes, err)
		}
	})
	t.Run("definite-failure-attempted-once", func(t *testing.T) {
		s, b := newTestStore(t)
		gcAliases(t, s, b, []int{0, 0, 2, 2}, []string{"", "key", "key", ""})
		for i := 0; i < 4; i++ {
			if err := s.Remove(ctx, fmt.Sprintf("/bucket/alias-%d", i)); err != nil {
				t.Fatal(err)
			}
		}
		b.mu.Lock()
		b.trashErr["account-a"] = errors.New("known rejection")
		b.mu.Unlock()
		if err := s.collect(ctx); err == nil || len(b.trashes) != 1 {
			t.Fatal("aliases retried the same physical deletion in one pass", b.trashes, err)
		}
	})
}

type blockedBody struct {
	started, closed chan struct{}
	once            sync.Once
	closes          int
	mu              sync.Mutex
}

func (r *blockedBody) Read([]byte) (int, error) {
	close(r.started)
	<-r.closed
	return 0, errors.New("private URL")
}
func (r *blockedBody) Close() error {
	r.once.Do(func() { r.mu.Lock(); r.closes++; r.mu.Unlock(); close(r.closed) })
	return nil
}
func TestStreamingCancellationAndInterruptedTrash(t *testing.T) {
	s, b := newTestStore(t)
	ctx := context.Background()
	put(t, s, "/bucket/file", "abcd")
	body := &blockedBody{started: make(chan struct{}), closed: make(chan struct{})}
	b.mu.Lock()
	b.openRange = func(context.Context, core.RemoteObject, int64, int64) (io.ReadCloser, error) { return body, nil }
	b.mu.Unlock()
	r, _, err := s.Open(ctx, "/bucket/file")
	if err != nil {
		t.Fatal(err)
	}
	read := make(chan error, 1)
	go func() { _, err := r.Read(make([]byte, 1)); read <- err }()
	select {
	case <-body.started:
	case <-time.After(5 * time.Second):
		t.Fatal("read did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- r.Close() }()
	select {
	case err := <-read:
		if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "private") {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel body")
	}
	if err = <-closed; err != nil {
		t.Fatal(err)
	}
	body.mu.Lock()
	closes := body.closes
	body.mu.Unlock()
	if closes != 1 {
		t.Fatal(closes)
	}
	o := objectAt(t, s, "/bucket/file")
	if err = s.Remove(ctx, "/bucket/file"); err != nil {
		t.Fatal(err)
	}
	s.mutationMu.Lock()
	claim, err := s.claimDeletion(ctx, domainOf(o.manifest.Parts[0].Remote))
	s.mutationMu.Unlock()
	if err != nil || claim == nil {
		t.Fatal(claim, err)
	}
	s = reopen(t, s)
	if err = s.collect(ctx); err != nil || len(b.trashes) != 0 {
		t.Fatal("interrupted trash replayed", err)
	}
	o, err = s.object(ctx, o.id)
	if err != nil || o.manifest.Parts[0].TrashState != "uncertain" {
		t.Fatal(o, err)
	}
}
