// Package core defines the shared filesystem, Google backend, and HTTP contracts.
package core

import (
	"strconv"
	"time"
)

// Entry is persistent metadata for a logical VFS file or directory.
// Path is canonical, Size is logical file size, and Bucket is empty at root.
// Version changes when the entry's published representation changes.
type Entry struct {
	ID          int64     `json:"id"`
	Path        string    `json:"path"`
	Name        string    `json:"name"`
	IsDir       bool      `json:"isDir"`
	Size        int64     `json:"size"`
	ModTime     time.Time `json:"modTime"`
	Version     int64     `json:"version"`
	ContentType string    `json:"contentType"`
	Bucket      string    `json:"bucket"`
	CarrierKind string    `json:"-"`
}

// ETag returns a stable quoted validator based solely on ID and Version.
func (e Entry) ETag() string {
	return "\"" + strconv.FormatInt(e.ID, 10) + "-" + strconv.FormatInt(e.Version, 10) + "\""
}

// AlbumRef identifies an album within its owning account; ID alone is not a
// portable reference across accounts.
type AlbumRef struct {
	ID        string `json:"id"`
	AccountID string `json:"accountID"`
}

// Bucket maps one top-level VFS directory to an account-bound remote album.
// Album.AccountID is pinned at creation; an empty Album.ID means that binding
// is deferred until the first confirmed content commit.
type Bucket struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Album     AlbumRef  `json:"album"`
	CreatedAt time.Time `json:"createdAt"`
}

// SetupStatus reports only whether first-run administrator initialization is complete.
type SetupStatus struct {
	Initialized bool `json:"initialized"`
}

// AdminProfile is the public administrator identity; it contains no verifier
// or other authentication material.
type AdminProfile struct {
	Username string `json:"username"`
}

// Account is safe to expose in account lists; it contains no credentials.
type Account struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Selected bool   `json:"selected"`
}

// RemoteObject identifies an uploaded carrier, not the logical payload.
// ID is the media_key, SHA1 is the carrier's hexadecimal SHA-1, and DedupKey
// has backend-defined semantics. AccountID must accompany remote references.
type RemoteObject struct {
	ID        string `json:"id"`
	AccountID string `json:"accountID"`
	DedupKey  string `json:"dedupKey"`
	SHA1      string `json:"sha1"`
	Size      int64  `json:"size"`
}

// UploadSettings stores upload preferences. LargeFileFormat is "mp4" or "bmp".
// Part sizes are complete-carrier budgets in decimal bytes: MP4 permits
// 1,000,000,000..9,000,000,000 and BMP permits 100,000,000..150,000,000.
// Each selected budget is the actual splitting threshold, including framing.
// Logical file size has no fixed policy cap beyond the technical int64 limit.
type UploadSettings struct {
	LargeFileFormat string `json:"largeFileFormat"`
	MP4PartSize     int64  `json:"mp4PartSize"`
	BMPPartSize     int64  `json:"bmpPartSize"`
}

// UploadPolicy is the immutable selected settings snapshot for one upload.
// Format is "mp4" or "bmp"; PartSize is its complete-carrier part budget.
type UploadPolicy struct {
	Format   string `json:"format"`
	PartSize int64  `json:"partSize"`
}

// PartPlan is an immutable packing plan. Offset and Length address the logical
// source, Kind is "bmp" or "mp4", and Budget includes all carrier overhead.
type PartPlan struct {
	Index  int    `json:"index"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	Kind   string `json:"kind"`
	Budget int64  `json:"budget"`
}

// DownloadSource describes the immutable logical entry and its carrier segments.
// Segment URLs are private scoped capabilities, not general file metadata.
type DownloadSource struct {
	Entry    Entry             `json:"entry"`
	Segments []DownloadSegment `json:"segments"`
}

// DownloadSegment maps a logical range to one carrier. Offset and Size address
// logical payload bytes; PayloadOffset and CarrierSize address the carrier.
// URL must never be serialized as general JSON, logged, or persisted as a
// user preference.
type DownloadSegment struct {
	Offset        int64  `json:"offset"`
	Size          int64  `json:"size"`
	URL           string `json:"-"`
	PayloadOffset int64  `json:"payloadOffset"`
	CarrierSize   int64  `json:"carrierSize"`
}

// Carrier describes a local packed file and its logical payload byte range.
// Format is versioned (for example, "guludrive-bmp-v1"). SHA1 hashes the entire
// carrier, and Size is its total byte length. Path is an internal absolute
// spool path and is never serialized.
type Carrier struct {
	Path          string `json:"-"`
	Format        string `json:"format"`
	SHA1          string `json:"sha1"`
	Size          int64  `json:"size"`
	PayloadOffset int64  `json:"payloadOffset"`
	PayloadSize   int64  `json:"payloadSize"`
}

// ObjectPart records one immutable plan and its physical carrier, remote
// identity, and journal states. Carrier.Path is not serialized; local part
// paths are reconstructed from the job ID and plan index.
type ObjectPart struct {
	Plan        PartPlan     `json:"plan"`
	Carrier     Carrier      `json:"carrier"`
	Remote      RemoteObject `json:"remote"`
	UploadState string       `json:"uploadState"`
	AlbumState  string       `json:"albumState"`
	TrashState  string       `json:"trashState"`
}

// PartsManifest is the typed value of objects.parts for one logical object.
// Version is currently 1; Policy preserves the selected upload snapshot.
type PartsManifest struct {
	Version     int          `json:"version"`
	LogicalSize int64        `json:"logicalSize"`
	Policy      UploadPolicy `json:"policy"`
	Parts       []ObjectPart `json:"parts"`
	// AlbumStates keeps per-part receipts independently for every destination album.
	AlbumStates map[string][]string `json:"albumStates,omitempty"`
}

// UploadRequest supplies a journal operation ID, destination account/album,
// local carrier, and logical basename. The provider adds the carrier suffix;
// Upload does not attach it to the album.
type UploadRequest struct {
	OperationID string   `json:"operationID"`
	Album       AlbumRef `json:"album"`
	Carrier     Carrier  `json:"carrier"`
	Filename    string   `json:"filename"`
}

// WriteOptions carries publication preconditions and logical file metadata.
// Set both ExpectedID and ExpectedVersion for an ETag identity precondition.
// Implementations must validate supplied identity/version preconditions together
// against the entry being replaced when publishing, including after delete/recreate.
// ExpectedLength must be -1 when unknown; zero means an empty file.
type WriteOptions struct {
	ExpectedID      *int64     `json:"expectedID"`      // When non-nil, require this existing entry ID.
	ExpectedVersion *int64     `json:"expectedVersion"` // When non-nil, require this existing entry version.
	IfNoneMatch     bool       `json:"ifNoneMatch"`     // Require that the destination does not exist.
	ExpectedLength  int64      `json:"expectedLength"`
	ContentType     string     `json:"contentType"`
	ModTime         *time.Time `json:"modTime"` // When nil, let the filesystem choose the time.
}

// GDirectStatus reports the runtime GDirect mode and TLS-verified selected
// content IP, held only in memory. IP need not have served a file connection
// yet; it is empty without a selection, not a Worker node or client address.
type GDirectStatus struct {
	Enabled   bool   `json:"enabled"`
	IP        string `json:"ip"`
	Selecting bool   `json:"selecting"`
}

// Status reports service readiness without exposing backend credentials.
type Status struct {
	Version     string        `json:"version"`
	GoogleReady bool          `json:"googleReady"`
	GDirect     GDirectStatus `json:"gdirect"`
}

// Listing contains the requested entry and its directory children.
type Listing struct {
	Entry   Entry   `json:"entry"`
	Entries []Entry `json:"entries"`
}

// Transfer describes a logical copy or move request.
type Transfer struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Overwrite   bool   `json:"overwrite"`
	Recursive   bool   `json:"recursive"`
}
