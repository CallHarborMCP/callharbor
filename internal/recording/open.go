package recording

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Writer satisfies Sink: plaintext PCM goes through the STREAM writer.
var _ Sink = (*Writer)(nil)

// Path returns the on-disk location of a recording: recordings/<owner>/<id>.chrec
// under dataDir. Owner and id are validated so a value cannot escape the tree.
func Path(dataDir, ownerID, recordingID string) (string, error) {
	if !safeName(ownerID) || !safeName(recordingID) {
		return "", errors.New("recording: unsafe path component")
	}
	return filepath.Join(dataDir, "recordings", ownerID, recordingID+".chrec"), nil
}

// RelPath is the path stored in the database, relative to dataDir.
func RelPath(ownerID, recordingID string) string {
	return filepath.ToSlash(filepath.Join("recordings", ownerID, recordingID+".chrec"))
}

// safeName accepts only the id alphabet the rest of the system generates.
func safeName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// CreateFile makes the owner directory 0700 and creates the recording file
// exclusively with mode 0600. It returns the open writer.
func CreateFile(dataDir, ownerID, recordingID string, h Header, key []byte) (*Writer, error) {
	path, err := Path(dataDir, ownerID, recordingID)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("recording: create directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- 0700 is the directory mode; G302 applies to files, and the owner directory must be traversable
		return nil, fmt.Errorf("recording: directory mode: %w", err)
	}
	return NewWriter(path, h, key, recordingID, ownerID)
}
