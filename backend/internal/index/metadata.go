package index

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
)

// FileMeta holds metadata for a single indexed path.
// It is serialized as JSON for storage in the metadata LMDB database.
type FileMeta struct {
	ID               uint32 `json:"id"`
	Path             string `json:"path"`
	Filename         string `json:"filename"`
	OriginalFilename string `json:"original_filename"`
	Parent           string `json:"parent"`
	Extension        string `json:"extension"`
	IsDir            bool   `json:"is_dir"`
	Modified         int64  `json:"modified"`
	Depth            int    `json:"depth"`
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

// Uint32ToBytes converts a uint32 value into a 4-byte big-endian slice.
func Uint32ToBytes(val uint32) []byte {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, val)
	return buf
}

// BytesToUint32 parses a 4-byte big-endian slice into a uint32 value.
func BytesToUint32(buf []byte) uint32 {
	if len(buf) < 4 {
		return 0
	}
	return binary.BigEndian.Uint32(buf)
}

// MarshalPostingList serializes a slice of uint32 IDs to a compact big-endian binary buffer.
func MarshalPostingList(ids []uint32) []byte {
	buf := make([]byte, len(ids)*4)
	for i, id := range ids {
		binary.BigEndian.PutUint32(buf[i*4:], id)
	}
	return buf
}

// UnmarshalPostingList deserializes a big-endian binary buffer to a slice of uint32 IDs.
func UnmarshalPostingList(data []byte) ([]uint32, error) {
	if len(data)%4 != 0 {
		return nil, fmt.Errorf("invalid posting list data length: %d", len(data))
	}
	ids := make([]uint32, len(data)/4)
	for i := 0; i < len(ids); i++ {
		ids[i] = binary.BigEndian.Uint32(data[i*4:])
	}
	return ids, nil
}
