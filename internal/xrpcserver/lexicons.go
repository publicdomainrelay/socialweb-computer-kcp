package xrpcserver

import (
	"embed"
	"fmt"
	"maps"

	"github.com/bluesky-social/indigo/atproto/lexicon"
)

//go:embed lexicons/*.json
var lexiconFS embed.FS

func loadCatalog() (*lexicon.BaseCatalog, error) {
	c := lexicon.NewBaseCatalog()
	if err := c.LoadEmbedFS(lexiconFS); err != nil {
		return nil, fmt.Errorf("xrpcserver: loading the embedded lexicons: %w", err)
	}
	return c, nil
}

type inline struct {
	nsid    string
	obj     lexicon.SchemaObject
	catalog *lexicon.BaseCatalog
}

var _ lexicon.Catalog = inline{}

func (c inline) Resolve(ref string) (*lexicon.Schema, error) {
	if ref != c.nsid {
		return c.catalog.Resolve(ref)
	}
	return &lexicon.Schema{
		ID:  ref,
		Def: lexicon.SchemaRecord{Type: "record", Key: "any", Record: c.obj},
	}, nil
}

func objectOf(where string, def *lexicon.SchemaDef) (lexicon.SchemaObject, error) {
	if def == nil {
		return lexicon.SchemaObject{}, fmt.Errorf("xrpcserver: %s declares no schema", where)
	}
	obj, ok := def.Inner.(lexicon.SchemaObject)
	if !ok {
		return lexicon.SchemaObject{}, fmt.Errorf(
			"xrpcserver: %s is %T, not an object, and this transport validates objects only",
			where, def.Inner)
	}
	return obj, nil
}

func paramsAsObject(p *lexicon.SchemaParams) lexicon.SchemaObject {
	if p == nil {
		return lexicon.SchemaObject{Type: "object"}
	}
	return lexicon.SchemaObject{Type: "object", Properties: p.Properties, Required: p.Required}
}

func validate(where string, cat *lexicon.BaseCatalog, def lexicon.SchemaObject, data map[string]any) error {
	v := make(map[string]any, len(data)+1)
	maps.Copy(v, data)
	v["$type"] = where
	if err := lexicon.ValidateRecord(inline{nsid: where, obj: def, catalog: cat}, v, where, 0); err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	return nil
}
