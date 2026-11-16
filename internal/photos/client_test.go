package photos

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"guludrive/core"
	"guludrive/internal/media"
	_ "modernc.org/sqlite"
)

const fakeOAuth, fakeMaster, fakeBearer, testOrigin = "oauth2_4/offline-oauth-token", "offline-master-secret", "offline-bearer-secret", "http://127.0.0.1"

func testAccountDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS accounts(id TEXT PRIMARY KEY, email TEXT NOT NULL, android_id TEXT NOT NULL, master_token TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY, value TEXT NOT NULL);
	`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestLoginSQLPersistence(t *testing.T) {
	var bearers atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.URL.Path {
		case "/exchange":
			token := r.Form.Get("Token")
			if (token != fakeOAuth && token != fakeOAuth+"-other") || r.Form.Get("service") != "ac2dm" || r.Form.Get("client_sig") != playSig {
				t.Error("exchange fields")
			}
			email := "User@example.com"
			if token == fakeOAuth+"-other" {
				email = "Other@example.com"
			}
			fmt.Fprint(w, "Email="+email+"\nToken="+fakeMaster)
		case "/bearer":
			bearers.Add(1)
			if r.Form.Get("Token") != fakeMaster || r.Form.Get("app") != photosPackage || r.Form.Get("service") != photosService || r.Header.Get("device") != r.Form.Get("androidId") {
				t.Error("bearer fields")
			}
			fmt.Fprintf(w, "Auth=%s\nExpiry=%d\n", fakeBearer, time.Now().Add(time.Hour).Unix())
		case "/validate":
			b, _ := io.ReadAll(r.Body)
			if r.Header.Get("Authorization") != "Bearer "+fakeBearer || !bytes.Equal(b, blob(1, join(blob(1, blob(1, make([]byte, 20))), blob(2, nil)))) {
				t.Error("validation fields")
			}
		default:
			t.Error("unexpected authentication request")
		}
	}))
	defer s.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "store.db")
	db := testAccountDB(t, path)
	opts := AuthOptions{HTTPClient: s.Client(), Endpoints: AuthEndpoints{s.URL + "/exchange", s.URL + "/bearer", s.URL + "/validate"}}
	a, err := OpenAccounts(db, opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	account, err := a.Login(ctx, "  oauth_token="+fakeOAuth+"\n")
	if err != nil || account.Email != "user@example.com" || !account.Selected {
		t.Fatalf("login: %v", err)
	}
	other, err := a.Login(ctx, fakeOAuth+"-other")
	if err != nil || other.ID == account.ID {
		t.Fatal("second account login failed")
	}
	androidID, otherAndroidID := a.state.Accounts[account.ID].AndroidID, a.state.Accounts[other.ID].AndroidID
	if err = a.Select(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	if err = db.Ping(); err != nil {
		t.Fatal("accounts service closed the borrowed database")
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db = testAccountDB(t, path)
	a, err = OpenAccounts(db, opts)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := a.DefaultAccount(ctx)
	if err != nil || selected != account || bearers.Load() != 2 {
		t.Fatal("account selection was not reloaded locally")
	}
	var master, device string
	if err = db.QueryRow("SELECT master_token, android_id FROM accounts WHERE id = ?", account.ID).Scan(&master, &device); err != nil || master != fakeMaster || device != androidID {
		t.Fatal("persistent credentials were not retained")
	}
	token, err := a.BearerToken(ctx, account.ID)
	if err != nil || token != fakeBearer || bearers.Load() != 3 {
		t.Fatal("reopened account did not refresh its memory-only bearer")
	}

	// Fail the selection write after a targeted account write. Neither memory
	// nor SQL may publish a partial login or removal on storage failure.
	if _, err = db.Exec(`
		CREATE TRIGGER reject_auth_selection_update BEFORE UPDATE ON settings WHEN OLD.key = 'google.selected_account' BEGIN SELECT RAISE(ABORT, 'offline storage rejection'); END;
		CREATE TRIGGER reject_auth_selection_delete BEFORE DELETE ON settings WHEN OLD.key = 'google.selected_account' BEGIN SELECT RAISE(ABORT, 'offline storage rejection'); END;
	`); err != nil {
		t.Fatal(err)
	}
	if err = a.Select(ctx, other.ID); err == nil {
		t.Fatal("failed selection reported success")
	}
	if _, err = a.Login(ctx, fakeOAuth+"-other"); err == nil {
		t.Fatal("failed persistence reported a successful login")
	}
	if err = a.Remove(ctx, account.ID); err == nil {
		t.Fatal("failed removal reported success")
	}
	selected, err = a.DefaultAccount(ctx)
	if err != nil || selected != account || len(a.state.Accounts) != 2 || a.state.Accounts[other.ID].AndroidID != otherAndroidID {
		t.Fatal("failed SQL mutation changed in-memory accounts")
	}
	var storedSelected string
	var count int
	if err = db.QueryRow("SELECT count(*) FROM accounts").Scan(&count); err != nil || count != 2 {
		t.Fatal("failed removal changed persistent accounts")
	}
	if err = db.QueryRow("SELECT value FROM settings WHERE key = ?", selectedAccountKey).Scan(&storedSelected); err != nil || storedSelected != account.ID {
		t.Fatal("failed SQL mutation changed persistent selection")
	}
	if err = db.QueryRow("SELECT android_id FROM accounts WHERE id = ?", other.ID).Scan(&device); err != nil || device != otherAndroidID {
		t.Fatal("failed login replaced persistent credentials")
	}
	if _, err = db.Exec("DROP TRIGGER reject_auth_selection_update; DROP TRIGGER reject_auth_selection_delete;"); err != nil {
		t.Fatal(err)
	}
	if err = a.Select(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	if err = a.Remove(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = a.DefaultAccount(ctx); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal("removing the selected account retained a selection")
	}
	if _, err = OpenAccounts(db, opts); err != nil {
		t.Fatal("removal did not persist a valid remaining account state")
	}
	if err = db.Ping(); err != nil {
		t.Fatal("accounts operations closed the borrowed database")
	}
	for _, name := range []string{"auth", "key", "accounts.enc"} {
		if _, err = os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatal("accounts service created a legacy credential file or directory")
		}
	}
}

func TestLoginFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stage    LoginStage
		status   int
		body     string
		reason   string
		rejected bool
	}{
		{"master rejection", LoginStageExchangeMaster, 401, "Error=BadAuthentication", "BadAuthentication", true},
		{"master service failure", LoginStageExchangeMaster, 500, "Error=BadAuthentication", "BadAuthentication", false},
		{"bearer rejection", LoginStageExchangeBearer, 200, "Error=NeedsBrowser", "NeedsBrowser", true},
		{"validation rejection", LoginStageValidatePhotos, 403, "not an authentication field", "unknown", true},
		{"unknown Google reason", LoginStageExchangeMaster, 200, "Error=NeedPermission", "unknown", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				stage := map[string]LoginStage{"/exchange": LoginStageExchangeMaster, "/bearer": LoginStageExchangeBearer, "/validate": LoginStageValidatePhotos}[r.URL.Path]
				if stage == tc.stage {
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body+"\nInfo="+fakeOAuth+" "+fakeMaster+" "+fakeBearer+" fixture@example.com https://signed.invalid/?secret=fixture")
					return
				}
				switch stage {
				case LoginStageExchangeMaster:
					fmt.Fprint(w, "Email=User@example.com\nToken="+fakeMaster)
				case LoginStageExchangeBearer:
					fmt.Fprintf(w, "Auth=%s\nExpiry=%d\n", fakeBearer, time.Now().Add(time.Hour).Unix())
				default:
					t.Error("unexpected authentication step")
				}
			}))
			defer s.Close()
			db := testAccountDB(t, filepath.Join(t.TempDir(), "store.db"))
			a, err := OpenAccounts(db, AuthOptions{HTTPClient: s.Client(), Endpoints: AuthEndpoints{s.URL + "/exchange", s.URL + "/bearer", s.URL + "/validate"}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = a.Login(context.Background(), fakeOAuth)
			var login *LoginError
			if !errors.As(err, &login) || login.Stage != tc.stage || login.UpstreamStatus != tc.status || login.Reason != tc.reason || errors.Is(err, core.ErrUnauthorized) != tc.rejected {
				t.Fatalf("authentication classification: %v", err)
			}
			for cause := err; cause != nil; cause = errors.Unwrap(cause) {
				for _, secret := range []string{fakeOAuth, fakeMaster, fakeBearer, "fixture@example.com", "signed.invalid", "NeedPermission"} {
					if strings.Contains(cause.Error(), secret) {
						t.Fatal("authentication error leaked upstream data")
					}
				}
			}
			accounts, err := a.Accounts(context.Background())
			if err != nil || len(accounts) != 0 {
				t.Fatal("failed login persisted an account")
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type testTokens struct{}

func (testTokens) DefaultAccount(ctx context.Context) (core.Account, error) {
	return core.Account{ID: "different-default"}, ctx.Err()
}
func (testTokens) BearerToken(ctx context.Context, _ string) (string, error) {
	return fakeBearer, ctx.Err()
}
func (testTokens) InvalidateBearer(ctx context.Context, _, _ string) error { return ctx.Err() }
func testClient(t *testing.T, rt roundTripFunc) *Client {
	t.Helper()
	c, err := New(testTokens{}, ClientOptions{HTTPClient: &http.Client{Transport: rt}, Endpoints: Endpoints{testOrigin + "/upload", testOrigin + "/commit", testOrigin + "/album", testOrigin + "/add", testOrigin + "/download", testOrigin + "/trash", testOrigin}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func bmpFixture(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "fixture.bmp")
	payload := []byte("\x00\xffpayload")
	if err := os.WriteFile(src, payload, 0600); err != nil {
		t.Fatal(err)
	}
	p, err := media.New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.PackPart(context.Background(), src, dst, core.PartPlan{Length: int64(len(payload)), Kind: "bmp", Budget: media.MaxBMPCarrierSize}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A small ISO BMFF fixture suffices here: the backend checks the first box,
// while the production packer owns the sealed carrier framing.
func mp4Fixture() []byte {
	return []byte("\x00\x00\x00\x18ftypisom\x00\x00\x02\x00isommp42\x00\x00\x00\x10mdatpayload!")
}

func TestProtocol(t *testing.T) {
	for _, tc := range []struct {
		name, mime, suffix string
		data               []byte
	}{
		{"BMP", "image/bmp", ".bmp", bmpFixture(t)},
		{"MP4", "video/mp4", ".mp4", mp4Fixture()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			data := tc.data
			sum := sha1.Sum(data)
			fallbackDedup := base64.RawURLEncoding.EncodeToString(sum[:])
			finalize := join(integer(1, 2), blob(2, []byte("opaque-token")), integer(9, 1))
			var compressedFinalize bytes.Buffer
			gz := gzip.NewWriter(&compressedFinalize)
			if _, err := gz.Write(finalize); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			var calls []string
			c := testClient(t, func(r *http.Request) (*http.Response, error) {
				calls = append(calls, r.Method+" "+r.URL.Path)
				var b []byte
				if r.Body != nil {
					b, _ = io.ReadAll(r.Body)
					r.Body.Close()
				}
				var raw []byte
				h := make(http.Header)
				auth := r.Header.Get("Authorization")
				if (r.URL.Path == "/original" && auth != "") || (r.URL.Path != "/original" && auth != "Bearer "+fakeBearer) {
					t.Error("RPC/content authorization isolation")
				}
				switch r.URL.Path {
				case "/album":
					// Literal wire layout from gotohp/gpmc CreateAlbumRequest: fields
					// 1=name, 2=Unix seconds, 3=1, repeated 4={1:{1:media_key}},
					// 6={}, 7.1=3, 8=device. This golden includes the confirmed key;
					// expected bytes do not use the request builder under test.
					prefix := []byte("\x0a\x06bucket\x10")
					suffix := []byte("\x18\x01\x22\x09\x0a\x07\x0a\x05media\x32\x00\x3a\x02\x08\x03\x42\x14\x1a\x08Pixel XL\x22\x06Google\x28\x1c")
					if !bytes.HasPrefix(b, prefix) {
						t.Error("album request prefix")
					} else {
						seconds, n := protowire.ConsumeVarint(b[len(prefix):])
						if n < 0 || !bytes.Equal(b[len(prefix)+max(n, 0):], suffix) || seconds < uint64(time.Now().Add(-time.Minute).Unix()) || seconds > uint64(time.Now().Add(time.Minute).Unix()) {
							t.Error("album request fields")
						}
					}
					raw = []byte("\x0a\x07\x0a\x05album")
				case "/upload":
					if r.Method == "POST" {
						if r.Header.Get("X-Upload-Content-Type") != tc.mime || r.Header.Get("X-Upload-Content-Length") != fmt.Sprint(len(data)) || r.Header.Get("X-Goog-Hash") != "sha1="+base64.StdEncoding.EncodeToString(sum[:]) {
							t.Error("carrier token MIME, size or SHA-1")
						}
						h.Set("X-GUploader-UploadID", "upload/id+?=")
					} else {
						if r.Method != "PUT" || r.Header.Get("Content-Type") != tc.mime || !bytes.Equal(b, data) || r.URL.Query().Get("upload_id") != "upload/id+?=" {
							t.Error("carrier bytes")
						}
						raw = compressedFinalize.Bytes()
						h.Set("Content-Encoding", "gzip")
					}
				case "/commit":
					if name, err := textAt(b, 1, 2); err != nil || name != "file.bin"+tc.suffix {
						t.Error("carrier upload filename")
					}
					blueprint, _ := bytesAt(b, 1)
					fs, _ := fields(blueprint)
					original := false
					for _, field := range fs {
						if field.num == 7 && field.typ == protowire.VarintType && field.value == 3 {
							original = true
						}
					}
					if got, _ := bytesAt(b, 2); !original || !bytes.Equal(got, device()) {
						t.Error("original-quality commit or device fields changed")
					}
					got, err := bytesAt(b, 1, 1)
					if err != nil || !bytes.Equal(got, finalize) {
						t.Error("opaque finalize envelope changed")
					}
					// The media key is confirmed even when optional field 1.3.2 is
					// an unknown opaque payload rather than our known metadata shape.
					raw = blob(1, blob(3, join(str(1, "media"), blob(2, []byte{0xff}))))
				case "/add":
					if id, err := textAt(b, 1); err != nil || id != "media" {
						t.Error("attachment not deduplicated")
					}
				case "/download":
					if tc.suffix == ".mp4" {
						raw = blob(1, blob(5, blob(3, str(5, testOrigin+"/original"))))
					} else {
						raw = blob(1, blob(5, blob(2, join(str(5, testOrigin+"/edited"), str(6, testOrigin+"/original")))))
					}
				case "/original":
					if r.Header.Get("Range") != fmt.Sprintf("bytes=0-%d", len(data)-1) || r.Header.Get("Accept-Encoding") != "identity" {
						t.Error("whole-carrier range request")
					}
					raw = data
				case "/trash":
				default:
					t.Error("unexpected request or edited download")
				}
				return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(bytes.NewReader(raw)), ContentLength: int64(len(raw))}, nil
			})
			path := filepath.Join(t.TempDir(), "carrier.carrier")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			object, err := c.Upload(ctx, core.UploadRequest{Album: core.AlbumRef{AccountID: "account"}, Filename: "file.bin", Carrier: core.Carrier{Path: path, Size: int64(len(data)), SHA1: fmt.Sprintf("%x", sum)}})
			if err != nil || object.ID != "media" || object.AccountID != "account" || object.DedupKey != fallbackDedup {
				t.Fatalf("confirmed upload with optional opaque metadata: %v", err)
			}
			album, err := c.CreateAlbum(ctx, "bucket", []core.RemoteObject{object})
			if err != nil || album.AccountID != object.AccountID || strings.Join(calls, ",") != "POST /upload,PUT /upload,POST /commit,POST /album" {
				t.Fatalf("upload must confirm media before album creation on its pinned account: %v", err)
			}
			if err := c.AddToAlbum(ctx, album, []core.RemoteObject{object, object}); err != nil {
				t.Fatal(err)
			}
			wrong := object
			wrong.AccountID = "other"
			if err := c.AddToAlbum(ctx, album, []core.RemoteObject{wrong}); !errors.Is(err, core.ErrUnsupported) {
				t.Fatal("cross-account attachment accepted")
			}
			r, err := c.OpenRange(ctx, object, 0, object.Size)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(r)
			r.Close()
			if err != nil || !bytes.Equal(got, data) || c.Trash(ctx, object) != nil {
				t.Fatal("original-byte download/trash roundtrip")
			}
		})
	}
}

func TestDownloadSources(t *testing.T) {
	data := []byte("format-independent range data\x00\xff")
	const offset, length = int64(14), int64(7)
	original := blob(1, blob(5, blob(2, join(str(5, testOrigin+"/edited"), str(6, testOrigin+"/carrier")))))
	for _, tc := range []struct {
		name         string
		raw          []byte
		refuse, full bool
	}{
		{"original image without SHA1", original, false, false},
		{"original video", blob(1, blob(5, blob(3, str(5, testOrigin+"/carrier")))), false, false},
		{"video ignores Range", blob(1, blob(5, blob(3, str(5, testOrigin+"/carrier")))), false, true},
		{"video wins over image", blob(1, blob(5, join(blob(2, str(6, testOrigin+"/unused-image")), blob(3, str(5, testOrigin+"/carrier"))))), false, false},
		{"empty video uses image", blob(1, blob(5, join(blob(2, str(6, testOrigin+"/carrier")), blob(3, str(5, ""))))), false, false},
		{"malformed video forbids image fallback", blob(1, blob(5, join(blob(2, str(6, testOrigin+"/carrier")), blob(3, str(5, "https://%"))))), true, false},
		{"untrusted video forbids image fallback", blob(1, blob(5, join(blob(2, str(6, testOrigin+"/carrier")), blob(3, str(5, "http://outside.invalid/video"))))), true, false},
		{"edited only refused", blob(1, blob(5, blob(2, str(5, testOrigin+"/edited")))), true, false},
		{"no URL scanning", blob(1, blob(5, blob(3, str(6, testOrigin+"/carrier")))), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rpcs, gets := 0, 0
			c := testClient(t, func(r *http.Request) (*http.Response, error) {
				status, raw := http.StatusOK, tc.raw
				h := http.Header{"Set-Cookie": {"rpc-session=fixture; Path=/"}}
				if r.Method == "GET" {
					gets++
					if r.Header.Get("Range") != "bytes=14-20" || r.Header.Get("Accept-Encoding") != "identity" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
						t.Error("range/encoding or credential isolation lost on content request")
					}
				}
				switch r.Method + " " + r.URL.Path {
				case "POST /download":
					rpcs++
					if r.Header.Get("Authorization") != "Bearer "+fakeBearer {
						t.Error("prepare RPC missing authorization")
					}
				case "GET /carrier":
					status, raw = http.StatusTemporaryRedirect, nil
					h.Set("Location", "/range")
				case "GET /range":
					if tc.full {
						status, raw = http.StatusOK, data
					} else {
						status, raw = http.StatusPartialContent, data[offset:offset+length]
						h.Set("Content-Range", fmt.Sprintf("bytes 14-20/%d", len(data)))
					}
				default:
					t.Fatal("unexpected request or edited download")
				}
				// Extra underlying bytes must neither leak nor trigger an EOF probe.
				body := io.Reader(bytes.NewReader(raw))
				if status == http.StatusPartialContent {
					body = bytes.NewReader(append(bytes.Clone(raw), 0x7f))
				}
				return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(body), ContentLength: int64(len(raw))}, nil
			})
			object := core.RemoteObject{ID: "media", AccountID: "account", Size: int64(len(data))}
			source, err := c.DownloadURL(context.Background(), object)
			if tc.refuse {
				if source != "" || err == nil || rpcs != 1 || gets != 0 {
					t.Fatal("unsupported source must be refused before GET", err)
				}
				return
			}
			if err != nil || source != testOrigin+"/carrier" || rpcs != 1 || gets != 0 {
				t.Fatal("URL preparation must resolve only the original without reading content", err)
			}
			r, err := c.OpenRange(context.Background(), object, offset, length)
			if err != nil || rpcs != 2 || gets != 2 {
				t.Fatal("ranged original source and redirect", err)
			}
			defer r.Close()
			got, err := io.ReadAll(r)
			if err != nil || !bytes.Equal(got, data[offset:offset+length]) {
				t.Fatal("bounded original range", err)
			}
		})
	}
}

func TestUploadCarrierValidation(t *testing.T) {
	data := bmpFixture(t)
	p := filepath.Join(t.TempDir(), "carrier.bmp")
	calls := 0
	c := testClient(t, func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("unexpected upload request")
	})
	req := core.UploadRequest{Album: core.AlbumRef{AccountID: "account"}, Filename: "file", Carrier: core.Carrier{Path: p, Size: int64(len(data))}}
	data[0] = 'N'
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Upload(context.Background(), req); err == nil {
		t.Fatal("non-BMP carrier accepted")
	}
	data[0] = 'B'
	binary.LittleEndian.PutUint32(data[2:6], uint32(len(data)-1))
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Upload(context.Background(), req); err == nil {
		t.Fatal("BMP declared size mismatch accepted")
	}
	req.Carrier.Size = maxVideoBytes + 1
	if _, err := c.Upload(context.Background(), req); !errors.Is(err, core.ErrInvalid) || calls != 0 {
		t.Fatal("invalid carrier sent to Google", err)
	}
	binary.LittleEndian.PutUint32(data[2:6], uint32(len(data)))
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	req.Carrier.Size = int64(len(data))
	for _, name := range []string{"../file", "file\t", "file\u0085", strings.Repeat("x", 1021)} {
		req.Filename = name
		if _, err := c.Upload(context.Background(), req); !errors.Is(err, core.ErrInvalid) || calls != 0 {
			t.Fatal("invalid final carrier filename reached upload", err)
		}
	}
	req.Filename = "file"
	data = mp4Fixture()
	binary.BigEndian.PutUint32(data[:4], 36) // First box exceeds the bounded header.
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	req.Carrier.Size = int64(len(data))
	if _, err := c.Upload(context.Background(), req); !errors.Is(err, core.ErrInvalid) || calls != 0 {
		t.Fatal("invalid MP4 first box reached upload", err)
	}
	data = mp4Fixture()
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	c = testClient(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/upload" {
			t.Fatal("an incomplete PUT must not reach commit")
		}
		h := make(http.Header)
		h.Set("X-GUploader-UploadID", "upload")
		raw := join(integer(1, 2), str(2, "opaque-token"))
		return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(bytes.NewReader(raw))}, nil
	})
	if _, err := c.Upload(context.Background(), req); err == nil || calls != 2 {
		t.Fatal("PUT success without transmitting the complete carrier must not commit", err)
	}
}

func TestCarrierHeaderBounds(t *testing.T) {
	bmp := make([]byte, 32)
	copy(bmp, "BM")
	binary.LittleEndian.PutUint32(bmp[2:6], uint32(maxPhotoBytes))
	mp4 := mp4Fixture()[:32]
	for _, tc := range []struct {
		name         string
		header       []byte
		size         int64
		mime, suffix string
	}{
		{"BMP decimal limit", bmp, 150_000_000, "image/bmp", ".bmp"},
		{"BMP over limit", bmp, 150_000_001, "", ""},
		{"BMP too short", bmp, 53, "", ""},
		{"MP4 decimal limit", mp4, 9_000_000_000, "video/mp4", ".mp4"},
		{"MP4 over limit", mp4, 9_000_000_001, "", ""},
		{"MP4 shorter than first box", mp4, 23, "", ""},
		{"MP4 header truncated", mp4[:16], 40, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mime, suffix, err := carrierType(tc.header, tc.size)
			if mime != tc.mime || suffix != tc.suffix || (err != nil) != (tc.mime == "") {
				t.Fatal("carrier classification or decimal limit", mime, suffix, err)
			}
		})
	}
}

func TestDownloadBounds(t *testing.T) {
	calls := 0
	c := testClient(t, func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("unexpected I/O")
	})
	object := core.RemoteObject{ID: "media", AccountID: "account", Size: 32}
	for _, bounds := range [][2]int64{{-1, 1}, {0, -1}, {33, 0}, {32, 1}, {1, 1<<63 - 1}} {
		if r, err := c.OpenRange(context.Background(), object, bounds[0], bounds[1]); r != nil || !errors.Is(err, core.ErrInvalid) {
			t.Fatal("invalid or overflowing range accepted", bounds, err)
		}
	}
	for _, invalid := range []core.RemoteObject{{ID: "media", Size: 32}, {AccountID: "account", Size: 32}, {ID: "media", AccountID: "account"}, {ID: "media", AccountID: "account", Size: c.maxDownload + 1}} {
		if r, err := c.OpenRange(context.Background(), invalid, 0, 0); r != nil || !errors.Is(err, core.ErrInvalid) {
			t.Fatal("invalid object accepted", err)
		}
		if source, err := c.DownloadURL(context.Background(), invalid); source != "" || !errors.Is(err, core.ErrInvalid) {
			t.Fatal("invalid URL preparation accepted", err)
		}
	}
	for _, offset := range []int64{0, object.Size} {
		r, err := c.OpenRange(context.Background(), object, offset, 0)
		if err != nil {
			t.Fatal(err)
		}
		var one [1]byte
		n, err := r.Read(one[:])
		if closeErr := r.Close(); n != 0 || err != io.EOF || closeErr != nil {
			t.Fatal("zero length must return closable EOF", err, closeErr)
		}
	}
	if calls != 0 {
		t.Fatal("invalid or empty range performed I/O")
	}
	if c.maxDownload != maxDownloadCarrierBytes {
		t.Fatal("existing-object download bound changed with current upload caps")
	}
	limited, err := New(testTokens{}, ClientOptions{MaxDownloadBytes: maxPhotoBytes})
	if err != nil {
		t.Fatal(err)
	}
	large := core.RemoteObject{ID: "media", AccountID: "account", Size: maxDownloadCarrierBytes}
	if _, err = limited.DownloadURL(context.Background(), large); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("explicit smaller bound was ignored", err)
	}
	if _, err = New(testTokens{}, ClientOptions{MaxDownloadBytes: maxDownloadCarrierBytes + 1}); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("oversized download bound accepted", err)
	}
	c.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method == "POST" {
			raw := blob(1, blob(5, blob(3, str(5, testOrigin+"/carrier"))))
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(raw))}, nil
		}
		if r.Header.Get("Range") != "bytes=9999999993-9999999999" {
			t.Error("large carrier range lost 64-bit decimal coordinates")
		}
		return &http.Response{StatusCode: 206, Header: http.Header{"Content-Range": {"bytes 9999999993-9999999999/10000000000"}}, ContentLength: 7, Body: io.NopCloser(strings.NewReader("payload"))}, nil
	})
	r, err := c.OpenRange(context.Background(), large, maxDownloadCarrierBytes-7, 7)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "payload" || calls != 2 {
		t.Fatal("10 GB metadata requires only the requested small range", err)
	}
}

type trackedDownloadBody struct {
	io.Reader
	closed    bool
	reads     int
	bytesRead int
	afterRead func(int)
}

func (b *trackedDownloadBody) Read(p []byte) (int, error) {
	b.reads++
	n, err := b.Reader.Read(p)
	b.bytesRead += n
	if b.afterRead != nil && n > 0 {
		b.afterRead(b.bytesRead)
	}
	return n, err
}
func (b *trackedDownloadBody) Close() error {
	b.closed = true
	if closer, ok := b.Reader.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func TestDownloadRangeFraming(t *testing.T) {
	const carrier = "0123456789abcdefghijklmnopqrstuv"
	for _, tc := range []struct {
		name, contentRange, encoding string
		status                       int
		length                       int64
		accept                       bool
	}{
		{"ignored Range trimmed", "", "", 200, 32, true},
		{"unknown full length", "", "identity", 200, -1, true},
		{"wrong full length", "", "", 200, 4, false},
		{"unexpected full bounds", "bytes 0-31/32", "", 200, 32, false},
		{"encoded full response", "", "gzip", 200, 32, false},
		{"missing bounds", "", "", 206, 4, false},
		{"wrong bounds", "bytes 13-16/32", "", 206, 4, false},
		{"wrong total", "bytes 12-15/33", "", 206, 4, false},
		{"wrong length", "bytes 12-15/32", "", 206, 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &trackedDownloadBody{Reader: strings.NewReader(carrier)}
			gets := 0
			c := testClient(t, func(r *http.Request) (*http.Response, error) {
				if r.Method == "POST" {
					raw := blob(1, blob(5, blob(2, str(6, testOrigin+"/carrier"))))
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(raw))}, nil
				}
				gets++
				if r.Header.Get("Range") != "bytes=12-15" {
					t.Error("content request must retain Range")
				}
				h := make(http.Header)
				if tc.contentRange != "" {
					h.Set("Content-Range", tc.contentRange)
				}
				if tc.encoding != "" {
					h.Set("Content-Encoding", tc.encoding)
				}
				return &http.Response{StatusCode: tc.status, Header: h, Body: body, ContentLength: tc.length}, nil
			})
			// Downloading ignores SHA-1 metadata, even when it is malformed.
			r, err := c.OpenRange(context.Background(), core.RemoteObject{ID: "media", AccountID: "account", Size: 32, SHA1: "not-a-content-check"}, 12, 4)
			if tc.accept {
				if err != nil || body.reads != 0 {
					t.Fatal("safe full-response framing must open without reading its prefix", err)
				}
				defer r.Close()
				got, err := io.ReadAll(r)
				if err != nil || string(got) != carrier[12:16] || body.bytesRead != 16 || gets != 1 {
					t.Fatal("fallback must skip the prefix, bound the payload and avoid tail probes or refetches", string(got), err)
				}
				if err := r.Close(); err != nil || !body.closed {
					t.Fatal("Close must immediately close the full response", err)
				}
				return
			}
			if r != nil {
				r.Close()
			}
			reason := "rejected"
			if tc.encoding != "" {
				reason = "unsupported_encoding"
			}
			var diagnostic *core.RemoteError
			if !errors.As(err, &diagnostic) || diagnostic.Operation != "download" || diagnostic.Stage != "http_response" || diagnostic.Reason != reason || !body.closed || body.reads != 0 || gets != 1 {
				t.Fatal("invalid framing must be rejected without exposing or reading the body", err)
			}
		})
	}
}

func TestDownloadTruncation(t *testing.T) {
	const carrier = "0123456789abcdefghijklmnopqrstuv"
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{"short full prefix", carrier[:11], "", 200},
		{"short full payload", carrier[:14], carrier[12:14], 200},
		{"short partial payload", carrier[12:14], carrier[12:14], 206},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &trackedDownloadBody{Reader: strings.NewReader(tc.body)}
			c := testClient(t, func(r *http.Request) (*http.Response, error) {
				if r.Method == "POST" {
					raw := blob(1, blob(5, blob(2, str(6, testOrigin+"/carrier"))))
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(raw))}, nil
				}
				h, length := make(http.Header), int64(32)
				if tc.status == http.StatusPartialContent {
					h.Set("Content-Range", "bytes 12-15/32")
					length = 4
				}
				return &http.Response{StatusCode: tc.status, Header: h, Body: body, ContentLength: length}, nil
			})
			r, err := c.OpenRange(context.Background(), core.RemoteObject{ID: "media", AccountID: "account", Size: 32}, 12, 4)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			got, err := io.ReadAll(r)
			if !errors.Is(err, io.ErrUnexpectedEOF) || string(got) != tc.want || !body.closed {
				t.Fatal("short framing must report unexpected EOF without exposing the prefix", string(got), err)
			}
		})
	}
}

func TestDownloadStreamingCancellation(t *testing.T) {
	const offset = 96 << 10
	const payload = "payload"
	for _, tc := range []struct {
		name       string
		status     int
		length     int64
		cancelSkip bool
	}{
		{"partial response cancellation", 206, 7, false},
		{"full response bounded close", 200, 2, false},
		{"full response prefix cancellation", 200, 7, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closed, prefixRead := make(chan struct{}), make(chan struct{})
			var gets atomic.Int32
			var origin string
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/download":
					raw := blob(1, blob(5, blob(2, str(6, origin+"/carrier"))))
					_, _ = w.Write(raw)
				case "/carrier":
					gets.Add(1)
					if r.Header.Get("Range") != fmt.Sprintf("bytes=%d-%d", offset, offset+tc.length-1) || r.Header.Get("Accept-Encoding") != "identity" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
						t.Error("loopback range request or credential isolation")
					}
					length := int64(offset + len(payload))
					if tc.status == http.StatusPartialContent {
						w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+tc.length-1, length))
						length = tc.length
					}
					w.Header().Set("Content-Length", fmt.Sprint(length))
					w.WriteHeader(tc.status)
					w.(http.Flusher).Flush()
					if tc.status == http.StatusOK {
						prefix := offset
						if tc.cancelSkip {
							prefix -= 2
						}
						_, _ = io.WriteString(w, strings.Repeat("x", prefix))
					}
					if !tc.cancelSkip {
						_, _ = io.WriteString(w, payload[:2])
					}
					w.(http.Flusher).Flush()
					<-r.Context().Done() // Never send the tail; Close/cancel must unblock it.
					close(closed)
				default:
					t.Error("unexpected loopback request")
				}
			}))
			defer s.Close()
			origin = s.URL
			c, err := New(testTokens{}, ClientOptions{HTTPClient: s.Client(), Endpoints: Endpoints{Download: s.URL + "/download", Content: s.URL}})
			if err != nil {
				t.Fatal(err)
			}
			if tc.cancelSkip {
				transport := c.client.Transport
				c.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
					resp, err := transport.RoundTrip(req)
					if err == nil && req.URL.Path == "/carrier" {
						resp.Body = &trackedDownloadBody{Reader: resp.Body, afterRead: func(n int) {
							if n == offset-2 {
								close(prefixRead)
							}
						}}
					}
					return resp, err
				})
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			r, err := c.OpenRange(ctx, core.RemoteObject{ID: "media", AccountID: "account", Size: offset + int64(len(payload))}, offset, tc.length)
			if err != nil {
				t.Fatal("open must return after headers, without reading the blocked body", err)
			}
			defer r.Close()
			first := make(chan error, 1)
			go func() {
				var part [2]byte
				_, err := io.ReadFull(r, part[:])
				if err == nil && string(part[:]) != payload[:2] {
					err = errors.New("wrong range prefix")
				}
				first <- err
			}()
			if tc.cancelSkip {
				select {
				case <-prefixRead:
				case <-ctx.Done():
					t.Fatal("prefix did not stream before cancellation")
				}
				cancel()
			}
			select {
			case err := <-first:
				if (!tc.cancelSkip && err != nil) || (tc.cancelSkip && !errors.Is(err, context.Canceled)) {
					t.Fatal("first read must stream or preserve cancellation during prefix skip", err)
				}
			case <-ctx.Done():
				if !tc.cancelSkip {
					t.Fatal("first small read buffered the blocked tail")
				}
				select {
				case err := <-first:
					if !errors.Is(err, context.Canceled) {
						t.Fatal("prefix read lost cancellation", err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("cancellation did not unblock prefix skip")
				}
			}
			if tc.status == http.StatusPartialContent {
				cancel()
			} else if !tc.cancelSkip {
				var one [1]byte
				if n, err := r.Read(one[:]); n != 0 || err != io.EOF {
					t.Fatal("bounded read probed the blocked tail", err)
				}
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-closed:
			case <-time.After(2 * time.Second):
				t.Fatal("Close/cancellation did not close the content response")
			}
			if gets.Load() != 1 {
				t.Fatal("stream must use one content request, not per-buffer refetches")
			}
		})
	}
}

func TestCreateAlbumBindings(t *testing.T) {
	calls := 0
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		calls++
		raw, _ := io.ReadAll(r.Body)
		prefix := []byte("\x0a\x06bucket\x10")
		// Reference-derived repeated field 4: {1:{1:"media"}},
		// {1:{1:"other"}}. Compare literal bytes, not our request helpers.
		suffix := []byte("\x18\x01\x22\x09\x0a\x07\x0a\x05media\x22\x09\x0a\x07\x0a\x05other\x32\x00\x3a\x02\x08\x03\x42\x14\x1a\x08Pixel XL\x22\x06Google\x28\x1c")
		if !bytes.HasPrefix(raw, prefix) {
			t.Error("album golden prefix")
		} else if _, n := protowire.ConsumeVarint(raw[len(prefix):]); n < 0 || !bytes.Equal(raw[len(prefix)+max(n, 0):], suffix) {
			t.Error("album golden repeated media bindings")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("\x0a\x07\x0a\x05album"))}, nil
	})
	objects := []core.RemoteObject{{ID: "media", AccountID: "pinned"}, {ID: "other", AccountID: "pinned"}}
	for _, invalid := range [][]core.RemoteObject{nil, make([]core.RemoteObject, 501), {{AccountID: "pinned"}}, {{ID: "media", AccountID: "unsafe account"}}, {objects[0], {ID: "other", AccountID: "different"}}} {
		if _, err := c.CreateAlbum(context.Background(), "bucket", invalid); err == nil || calls != 0 {
			t.Fatal("invalid album inputs performed a mutation", err)
		}
	}
	if err := c.AddToAlbum(context.Background(), core.AlbumRef{AccountID: "pinned"}, objects); !errors.Is(err, core.ErrInvalid) || calls != 0 {
		t.Fatal("AddToAlbum requires an explicit album ID", err)
	}
	album, err := c.CreateAlbum(context.Background(), "bucket", objects)
	if err != nil || calls != 1 || album.AccountID != "pinned" {
		t.Fatal("CreateAlbum must honor the objects' account, not the selected default", album, err)
	}
}

func TestCreateAlbumResponses(t *testing.T) {
	compress := func(raw []byte) []byte {
		t.Helper()
		var b bytes.Buffer
		w := gzip.NewWriter(&b)
		if _, err := w.Write(raw); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	album := []byte("\x0a\x07\x0a\x05album") // CreateAlbumResponse.album.id = field 1.1.
	damaged := compress(album)
	damaged[len(damaged)-1] ^= 0xff // Corrupt the gzip trailer, not just its header.
	oversized := bytes.Repeat([]byte("x"), (4<<20)+1)
	for _, tc := range []struct {
		name, encoding, stage, reason string
		status                        int
		body                          []byte
		uncertain                     bool
	}{
		{name: "plain success", status: 200, encoding: "identity", body: album},
		{name: "gzip success", status: 200, encoding: "gzip", body: compress(album)},
		{name: "bad request", status: 400, stage: "http_response", reason: "rejected"},
		{name: "rate limited", status: 429, stage: "http_response", reason: "rejected"},
		{name: "auth rejection", status: 401, stage: "http_response", reason: "rejected"},
		{name: "request timeout", status: 408, stage: "http_response", reason: "rejected", uncertain: true},
		{name: "redirect refused", status: 307, stage: "http_response", reason: "rejected", uncertain: true},
		{name: "server failure", status: 500, stage: "http_response", reason: "rejected", uncertain: true},
		{name: "missing identity", status: 200, stage: "decode_album_id", reason: "missing_album_id", uncertain: true},
		{name: "invalid protobuf", status: 200, body: []byte{0xff}, stage: "decode_response", reason: "invalid_protobuf", uncertain: true},
		{name: "damaged gzip", status: 200, encoding: "gzip", body: damaged, stage: "read_response", reason: "invalid_gzip", uncertain: true},
		{name: "decoded bound", status: 200, encoding: "gzip", body: compress(oversized), stage: "read_response", reason: "response_too_large", uncertain: true},
		{name: "wire bound", status: 200, body: oversized, stage: "read_response", reason: "response_too_large", uncertain: true},
		{name: "unknown encoding", status: 200, encoding: "br", body: album, stage: "read_response", reason: "unsupported_encoding", uncertain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Accept-Encoding") != "gzip" {
					t.Error("RPC must explicitly negotiate gzip")
				}
				w.Header().Set("Content-Encoding", tc.encoding)
				w.Header().Set("Location", "/redirected") // A 307 must not replay here.
				w.WriteHeader(tc.status)
				_, _ = w.Write(tc.body)
			}))
			defer s.Close()
			c, err := New(testTokens{}, ClientOptions{HTTPClient: s.Client(), Endpoints: Endpoints{CreateAlbum: s.URL}})
			if err != nil {
				t.Fatal(err)
			}
			a, err := c.CreateAlbum(context.Background(), "bucket", []core.RemoteObject{{ID: "media", AccountID: "account"}})
			if calls.Load() != 1 || errors.Is(err, core.ErrUncertain) != tc.uncertain || errors.Is(err, core.ErrUnauthorized) != (tc.status == 401) {
				t.Fatalf("mutation classification or replay: %v", err)
			}
			if tc.reason == "" {
				if err != nil || a.ID != "album" || a.AccountID != "account" {
					t.Fatalf("album response: %v", err)
				}
				return
			}
			var diagnostic *core.RemoteError
			if !errors.As(err, &diagnostic) || diagnostic.Operation != "create_album" || diagnostic.Stage != tc.stage || diagnostic.UpstreamStatus != tc.status || diagnostic.Reason != tc.reason {
				t.Fatalf("album diagnostic: %v", err)
			}
		})
	}
	c := testClient(t, func(*http.Request) (*http.Response, error) {
		return nil, errors.New(fakeBearer + " https://signed.invalid/?secret=fixture")
	})
	_, err := c.CreateAlbum(context.Background(), "bucket", []core.RemoteObject{{ID: "media", AccountID: "account"}})
	var diagnostic *core.RemoteError
	if !errors.As(err, &diagnostic) || !errors.Is(err, core.ErrUncertain) || diagnostic.Stage != "request" || diagnostic.UpstreamStatus != 0 || diagnostic.Reason != "network_error" || strings.Contains(err.Error(), fakeBearer) {
		t.Fatalf("network failure classification: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.CreateAlbum(ctx, "bucket", []core.RemoteObject{{ID: "media", AccountID: "account"}})
	if !errors.Is(err, context.Canceled) || errors.Is(err, core.ErrUncertain) {
		t.Fatalf("preflight cancellation is not an attempted mutation: %v", err)
	}
}

func TestRejectionBodyDiagnostics(t *testing.T) {
	secret := fakeOAuth + " " + fakeBearer + " fixture@example.com https://signed.invalid/?token=fixture"
	badRequest := join(blob(1, join(str(1, "4"), str(2, secret), str(3, "OFFLINE_BEARER_SECRET"))), blob(1, join(str(1, "fixture@example.com"), str(2, secret))))
	errorInfo := join(str(1, "SERVICE_DISABLED"), str(2, "fixture@example.com"), blob(3, join(str(1, "token"), str(2, fakeBearer))))
	status := join(integer(1, 3), str(2, secret), blob(3, join(str(1, "type.googleapis.com/google.rpc.BadRequest"), blob(2, badRequest))), blob(3, join(str(1, "type.googleapis.com/google.rpc.ErrorInfo"), blob(2, errorInfo))))
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(status); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		contentType, encoding, format string
		body                          []byte
	}{
		{"application/x-protobuf", "gzip", "protobuf", compressed.Bytes()},
		{"text/html", "identity", "html", []byte("<html>" + secret + "</html>")},
		{"application/x-protobuf", "gzip", "unknown", []byte("invalid gzip")},
	} {
		calls := 0
		c := testClient(t, func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {tc.contentType}, "Content-Encoding": {tc.encoding}}, Body: io.NopCloser(bytes.NewReader(tc.body))}, nil
		})
		_, err := c.CreateAlbum(context.Background(), "bucket", []core.RemoteObject{{ID: "media", AccountID: "account"}})
		var diagnostic *core.RemoteError
		if !errors.As(err, &diagnostic) || calls != 1 || errors.Is(err, core.ErrUncertain) || diagnostic.Stage != "http_response" || diagnostic.UpstreamStatus != 400 || diagnostic.BodyFormat != tc.format {
			t.Fatalf("rejection diagnostics/classification: %v", err)
		}
		if tc.format == "protobuf" {
			if diagnostic.RPCCode != 3 || diagnostic.ErrorReason != "SERVICE_DISABLED" || strings.Join(diagnostic.ViolationFields, ",") != "4" {
				t.Fatal("safe protobuf machine fields were lost")
			}
		} else if diagnostic.RPCCode != 0 || diagnostic.ErrorReason != "" || len(diagnostic.ViolationFields) != 0 {
			t.Fatal("non-protobuf body produced RPC diagnostics")
		}
		for _, private := range []string{fakeOAuth, fakeBearer, "fixture@example.com", "signed.invalid", "OFFLINE_BEARER_SECRET"} {
			if strings.Contains(diagnostic.Error(), private) {
				t.Fatal("rejection diagnostics exposed a message, description or metadata value")
			}
		}
	}
}

func TestReadOnlyDecodeFailure(t *testing.T) {
	for _, raw := range [][]byte{nil, {0xff}} {
		c := testClient(t, func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(raw))}, nil
		})
		_, err := c.OpenRange(context.Background(), core.RemoteObject{ID: "media", AccountID: "account", Size: 10}, 0, 10)
		var diagnostic *core.RemoteError
		if !errors.As(err, &diagnostic) || errors.Is(err, core.ErrUncertain) || diagnostic.Operation != "download" || diagnostic.Stage != "decode_response" || diagnostic.UpstreamStatus != 200 {
			t.Fatalf("read-only decoding must retain diagnostics without uncertainty: %v", err)
		}
	}
}

func TestOtherMutationRejections(t *testing.T) {
	data := bmpFixture(t)
	p := filepath.Join(t.TempDir(), "carrier.bmp")
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	album := core.AlbumRef{ID: "album", AccountID: "account"}
	object := core.RemoteObject{ID: "media", AccountID: "account", DedupKey: "dedup"}
	for _, status := range []int{401, 403, 429, 500} {
		c := testClient(t, func(r *http.Request) (*http.Response, error) {
			code, raw, h := status, []byte(nil), make(http.Header)
			if r.URL.Path == "/upload" {
				code = 200
				h.Set("X-GUploader-UploadID", "upload")
				if r.Method == "PUT" {
					_, _ = io.Copy(io.Discard, r.Body)
					raw = []byte("\x08\x02\x12\x01x")
				}
			}
			return &http.Response{StatusCode: code, Header: h, Body: io.NopCloser(bytes.NewReader(raw))}, nil
		})
		_, commitErr := c.Upload(context.Background(), core.UploadRequest{Album: album, Filename: "file", Carrier: core.Carrier{Path: p, Size: int64(len(data))}})
		for op, err := range map[string]error{"commit_upload": commitErr, "add_to_album": c.AddToAlbum(context.Background(), album, []core.RemoteObject{object}), "trash": c.Trash(context.Background(), object)} {
			var diagnostic *core.RemoteError
			if !errors.As(err, &diagnostic) || diagnostic.Operation != op || diagnostic.UpstreamStatus != status || errors.Is(err, core.ErrUncertain) != (status == 500) || errors.Is(err, core.ErrUnauthorized) != (status == 401 || status == 403) {
				t.Fatalf("%s HTTP %d: %v", op, status, err)
			}
		}
	}
}

type cancelBody struct {
	io.Reader
	cancel context.CancelFunc
}

func (b cancelBody) Close() error { b.cancel(); return nil }
func TestCancellationAndSecretSafeErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	c := testClient(t, func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: cancelBody{strings.NewReader(""), cancel}}, nil
	})
	object := core.RemoteObject{ID: "media", AccountID: "account", DedupKey: "dedup"}
	err := c.Trash(ctx, object)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, core.ErrUncertain) || !errors.Is(ErrUncertainCommit, core.ErrUncertain) || calls != 1 {
		t.Fatalf("completion cancellation: %v", err)
	}
	c.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New(fakeMaster + " https://signed.invalid/?token=secret")
	})
	err = c.Trash(context.Background(), object)
	if !errors.Is(err, core.ErrUncertain) || strings.Contains(err.Error(), fakeMaster) || strings.Contains(err.Error(), "signed.invalid") {
		t.Fatal("transport error leaked secrets or lost uncertainty")
	}
	authCtx, cancelAuth := context.WithCancel(context.Background())
	defer cancelAuth()
	db := testAccountDB(t, filepath.Join(t.TempDir(), "store.db"))
	a, err := OpenAccounts(db, AuthOptions{HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		cancelAuth()
		return nil, errors.New(fakeMaster + " https://signed.invalid/?token=secret")
	})}, Endpoints: AuthEndpoints{testOrigin + "/exchange", testOrigin + "/bearer", testOrigin + "/validate"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Login(authCtx, fakeOAuth)
	var login *LoginError
	if !errors.As(err, &login) || login.Stage != LoginStageExchangeMaster || login.UpstreamStatus != 0 || !errors.Is(err, context.Canceled) || errors.Is(err, core.ErrUnauthorized) || strings.Contains(err.Error(), fakeMaster) || strings.Contains(err.Error(), "signed.invalid") {
		t.Fatal("authentication cancellation lost its stage, cause or secret safety")
	}
}
