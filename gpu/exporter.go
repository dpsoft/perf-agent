package gpu

import (
	"encoding/json"
	"io"
)

func WriteJSONSnapshot(w io.Writer, snap Snapshot) error {
	enc := json.NewEncoder(w)
	return enc.Encode(snap)
}
