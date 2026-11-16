package core

import (
	"context"
	"io"
)

// FileSystem manages logical payloads and metadata using canonical, already
// URL-decoded absolute paths. Root contains buckets; nested directories are
// VFS metadata. Its owner closes the filesystem; consumers close read handles
// and finish write sessions with Commit or Abort.
type FileSystem interface {
	ListBuckets(ctx context.Context) ([]Bucket, error)
	Stat(ctx context.Context, path string) (Entry, error)
	ReadDir(ctx context.Context, path string) ([]Entry, error)

	// Mkdir at /bucket creates local metadata pinned to the selected account.
	// Its remote album is bound only after the first confirmed media upload.
	// Nested directories remain local metadata within that bucket.
	Mkdir(ctx context.Context, path string) error

	// Open exposes the logical payload, not its carrier bytes.
	Open(ctx context.Context, path string) (io.ReadSeekCloser, Entry, error)
	// PrepareDownload resolves immutable segment metadata and all known original
	// carrier URLs only when needed for Web Proxy, without reading carrier bytes.
	// It pins the logical object group while preparing and releases it on return;
	// this contract also permits future paged Worker manifest resolution.
	PrepareDownload(ctx context.Context, path string) (DownloadSource, error)
	BeginWrite(ctx context.Context, path string, options WriteOptions) (WriteSession, error)
	Copy(ctx context.Context, src, dst string, overwrite, recursive bool) (Entry, error)
	Move(ctx context.Context, src, dst string, overwrite bool) (Entry, error)
	Remove(ctx context.Context, path string) error
	Close() error
}

// WriteSession spools bytes locally until Commit publishes the new entry.
// Abort is idempotent and discards unpublished work. Close is deliberately not
// required; if an implementation provides it, Close must never publish data.
type WriteSession interface {
	io.Writer
	Commit(ctx context.Context) (Entry, error)
	Abort() error
}

// Backend manages account-bound albums and immutable carriers created by this
// service. The service owner manages backend lifetime; callers close readers
// returned by OpenRange.
type Backend interface {
	// CreateAlbum requires 1..500 confirmed objects from one valid account,
	// uses their pinned AccountID (not the current default), and includes them
	// in the new album. An unknown result must never be automatically replayed.
	CreateAlbum(ctx context.Context, name string, objects []RemoteObject) (AlbumRef, error)

	// Upload uses req.Album.AccountID; req.Album.ID may be empty for an unbound
	// bucket. It commits library media without attaching it to an album. The
	// caller journals the confirmed object before CreateAlbum or AddToAlbum.
	Upload(ctx context.Context, req UploadRequest) (RemoteObject, error)

	// AddToAlbum rejects objects belonging to another account; cross-account
	// copies require the caller to upload a new carrier to that account.
	AddToAlbum(ctx context.Context, album AlbumRef, objects []RemoteObject) error

	// DownloadURL prepares an allowlisted original carrier URL without reading
	// content bytes or forwarding account credentials to the returned URL.
	DownloadURL(ctx context.Context, object RemoteObject) (string, error)

	// OpenRange streams a bounded range in full-carrier coordinates, not logical
	// payload coordinates. Offset and length must be nonnegative and fit Size;
	// a zero length returns an empty reader without contacting the backend.
	OpenRange(ctx context.Context, object RemoteObject, offset, length int64) (io.ReadCloser, error)
	Trash(ctx context.Context, object RemoteObject) error
}

// Packer plans and wraps logical source ranges in current versioned carriers.
// BMP policy uses policy.PartSize for every part. MP4 policy selects one BMP
// when the whole source fits its 150,000,000-byte hard limit; otherwise it uses
// MP4 at policy.PartSize. The selected complete-carrier budget includes framing
// and is the actual splitting threshold, below the 9,000,000,000-byte MP4 cap.
// PackPart writes destinationPath using the immutable logical range and budget
// in part. Inspect checks bounded metadata for upload sealing/recovery, not
// download content integrity. The caller owns generated files and their cleanup.
type Packer interface {
	Plan(size int64, policy UploadPolicy) ([]PartPlan, error)
	PackPart(ctx context.Context, sourcePath, destinationPath string, part PartPlan) (Carrier, error)
	Inspect(path string) (Carrier, error)
}

// Accounts manages accounts without exposing stored credentials. Login accepts
// an OAuth token obtained by cookie token exchange; the auth service handles
// the exchange and stores secrets. Selecting an account changes defaults, not
// existing bucket album bindings. Consumers borrow this service; its owner
// manages the credential-store lifecycle.
type Accounts interface {
	Accounts(ctx context.Context) ([]Account, error)
	Login(ctx context.Context, oauthToken string) (Account, error)
	Select(ctx context.Context, id string) error
	Remove(ctx context.Context, id string) error
}
