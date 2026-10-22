package repo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
)

func marshalDAGCBOR(obj interface{ MarshalCBOR(w io.Writer) error }) ([]byte, cid.Cid, error) {
	var buf bytes.Buffer
	if err := obj.MarshalCBOR(&buf); err != nil {
		return nil, cid.Undef, err
	}
	data := buf.Bytes()
	c, err := cid.NewPrefixV1(cid.DagCBOR, multihash.SHA2_256).Sum(data)
	if err != nil {
		return nil, cid.Undef, err
	}
	return data, c, nil
}

func marshalCommit(commit *Commit) ([]byte, cid.Cid, error) {
	return marshalDAGCBOR(commit)
}

func parseCommitBlock(data []byte) (*Commit, error) {
	var commit Commit
	if err := commit.UnmarshalCBOR(bytes.NewReader(data)); err != nil {
		return nil, fmt.Errorf("parsing commit: %w", err)
	}
	return &commit, nil
}

func prepareRecord(collection syntax.NSID, record map[string]any) ([]byte, cid.Cid, map[string]any, error) {
	if record == nil {
		return nil, cid.Undef, nil, fmt.Errorf("%w: missing record", ErrInvalidRequest)
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, cid.Undef, nil, fmt.Errorf("%w: encoding record: %v", ErrInvalidRequest, err)
	}
	if len(raw) > atdata.MAX_JSON_RECORD_SIZE {
		return nil, cid.Undef, nil, fmt.Errorf("%w: record exceeds maximum size", ErrInvalidRequest)
	}
	obj, err := atdata.UnmarshalJSON(raw)
	if err != nil {
		return nil, cid.Undef, nil, fmt.Errorf("%w: invalid record data: %v", ErrInvalidRequest, err)
	}
	typ, ok := obj["$type"].(string)
	if !ok {
		return nil, cid.Undef, nil, fmt.Errorf("%w: record has no $type", ErrInvalidRequest)
	}
	if typ != collection.String() {
		return nil, cid.Undef, nil, fmt.Errorf("%w: record $type %q does not match collection %q", ErrInvalidRequest, typ, collection)
	}
	data, err := atdata.MarshalCBOR(obj)
	if err != nil {
		return nil, cid.Undef, nil, fmt.Errorf("%w: encoding record: %v", ErrInvalidRequest, err)
	}
	if len(data) > atdata.MAX_CBOR_RECORD_SIZE {
		return nil, cid.Undef, nil, fmt.Errorf("%w: record exceeds maximum size", ErrInvalidRequest)
	}
	c, err := cid.NewPrefixV1(cid.DagCBOR, multihash.SHA2_256).Sum(data)
	if err != nil {
		return nil, cid.Undef, nil, fmt.Errorf("hashing record: %w", err)
	}
	return data, c, obj, nil
}

func decodeRecord(data []byte) (map[string]any, error) {
	return atdata.UnmarshalCBOR(data)
}

func parseCID(raw string) (cid.Cid, error) {
	if raw == "" {
		return cid.Undef, nil
	}
	c, err := cid.Parse(raw)
	if err != nil {
		return cid.Undef, fmt.Errorf("parsing cid %q: %w", raw, err)
	}
	return c, nil
}

func parseCIDPtr(raw string) (*cid.Cid, error) {
	if raw == "" {
		return nil, nil
	}
	c, err := parseCID(raw)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func formatCIDPtr(c *cid.Cid) string {
	if c == nil {
		return ""
	}
	return c.String()
}

type storedOp struct {
	Action string `json:"action"`
	Path   string `json:"path"`
	CID    string `json:"cid,omitempty"`
	Prev   string `json:"prev,omitempty"`
}

func encodeOps(ops []CommitOp) ([]byte, error) {
	out := make([]storedOp, len(ops))
	for i, op := range ops {
		out[i] = storedOp{
			Action: string(op.Action),
			Path:   op.Path,
			CID:    formatCIDPtr(op.CID),
			Prev:   formatCIDPtr(op.Prev),
		}
	}
	return json.Marshal(out)
}

func encodeCIDs(cids []cid.Cid) ([]byte, error) {
	out := make([]string, len(cids))
	for i, c := range cids {
		out[i] = c.String()
	}
	return json.Marshal(out)
}

func decodeCIDs(data []byte) ([]cid.Cid, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var in []string
	if err := json.Unmarshal(data, &in); err != nil {
		return nil, fmt.Errorf("decoding cids: %w", err)
	}
	out := make([]cid.Cid, 0, len(in))
	for _, raw := range in {
		c, err := parseCID(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func decodeOps(data []byte) ([]CommitOp, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var in []storedOp
	if err := json.Unmarshal(data, &in); err != nil {
		return nil, fmt.Errorf("decoding ops: %w", err)
	}
	out := make([]CommitOp, 0, len(in))
	for _, op := range in {
		c, err := parseCIDPtr(op.CID)
		if err != nil {
			return nil, err
		}
		p, err := parseCIDPtr(op.Prev)
		if err != nil {
			return nil, err
		}
		out = append(out, CommitOp{
			Action: WriteAction(op.Action),
			Path:   op.Path,
			CID:    c,
			Prev:   p,
		})
	}
	return out, nil
}
