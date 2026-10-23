package apigen

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
)

var (
	endpointGoSuffixes   = []string{"URL", "URLs", "Url", "Urls", "URI", "URIs", "Uri", "Uris", "Path", "Paths", "File", "Files"}
	endpointJSONSuffixes = []string{"url", "urls", "uri", "uris", "path", "paths", "file", "files"}
	lowerCasingSuffixes  = []string{"Url", "Urls", "Uri", "Uris"}
)

func jsonEndpointName(name string) bool {
	lower := strings.ToLower(name)
	for _, s := range endpointJSONSuffixes {
		if lower == s {
			return true
		}
		if !strings.HasSuffix(lower, s) {
			continue
		}
		at := len(name) - len(s)
		if c := name[at]; c >= 'A' && c <= 'Z' {
			return true
		}
	}
	return false
}

func hasSuffix(name string, suffixes []string) bool {
	for _, s := range suffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

func endpointName(goName, jsonName string) bool {
	return hasSuffix(goName, endpointGoSuffixes) || jsonEndpointName(jsonName)
}

func (b *builder) checkCasing(t reflect.Type) error {
	var errs []error
	for f := range t.Fields() {
		if f.Anonymous {
			continue
		}
		if hasSuffix(f.Name, lowerCasingSuffixes) {
			errs = append(errs, fmt.Errorf("%s.%s: write the initialism in capitals (URL, URI)", t.Name(), f.Name))
		}
	}
	return errors.Join(errs...)
}

func (b *builder) checkSpecEndpoints(root reflect.Type) error {
	spec, ok := root.FieldByName("Spec")
	if !ok {
		return nil
	}
	var errs []error
	b.walkEndpoints(spec.Type, "spec", map[reflect.Type]bool{}, &errs)
	return errors.Join(errs...)
}

func (b *builder) reportOnce(key string) bool {
	if b.reported == nil {
		b.reported = map[string]bool{}
	}
	if b.reported[key] {
		return false
	}
	b.reported[key] = true
	return true
}

func (b *builder) walkEndpoints(t reflect.Type, path string, seen map[reflect.Type]bool, errs *[]error) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || t.PkgPath() != b.pkgPath || seen[t] {
		return
	}
	seen[t] = true
	defer delete(seen, t)
	for f := range t.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if f.Anonymous && name == "" {
			b.walkEndpoints(f.Type, path, seen, errs)
			continue
		}
		at := path + "." + name
		tag, err := parseTag(f.Tag.Get("apigen"))
		if err == nil && !tag.UserEndpoint && endpointName(f.Name, name) && b.reportOnce(t.Name()+"."+f.Name) {
			*errs = append(*errs, fmt.Errorf("%s (%s.%s) is a URL, URI, path or file in spec: publish the address in "+
				"the producer's status.address and reference the producer, or tag it apigen:%q when it is user input",
				at, t.Name(), f.Name, userEndpointTag))
		}
		b.walkEndpoints(f.Type, at, seen, errs)
	}
}
