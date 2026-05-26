package index

import "encoding/json"

// FileMeta holds metadata for a single indexed path.
// It is serialized as JSON for storage in the paths LMDB database.
type FileMeta struct {
	Path      string `json:"path"`
	Filename  string `json:"filename"`
	Parent    string `json:"parent"`
	Extension string `json:"extension"`
	IsDir     bool   `json:"is_dir"`
	Modified  int64  `json:"modified"`
	Depth     int    `json:"depth"`
}

// Marshal serializes FileMeta to JSON bytes.
func (m *FileMeta) Marshal() ([]byte, error) {
	return json.Marshal(m)
}

// UnmarshalFileMeta deserializes JSON bytes into a FileMeta.
func UnmarshalFileMeta(data []byte) (*FileMeta, error) {
	var m FileMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// PathSet is a JSON-serializable set of paths stored in the names
// and trigrams LMDB databases.
type PathSet []string

// Marshal serializes PathSet to JSON bytes.
func (ps PathSet) Marshal() ([]byte, error) {
	return json.Marshal(ps)
}

// UnmarshalPathSet deserializes JSON bytes into a PathSet.
func UnmarshalPathSet(data []byte) (PathSet, error) {
	var ps PathSet
	if err := json.Unmarshal(data, &ps); err != nil {
		return nil, err
	}
	return ps, nil
}
