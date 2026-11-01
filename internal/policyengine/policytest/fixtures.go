package policytest

import (
	"embed"
	"encoding/json"
	"fmt"
)

//go:embed testdata
var fixtures embed.FS

func WorkflowFixture(name string) string {
	data, err := fixtures.ReadFile("testdata/workflows/" + name + ".yml")
	if err != nil {
		panic(fmt.Sprintf("policytest: no workflow fixture %s: %v", name, err))
	}
	return string(data)
}

func RecordFixture(name string) map[string]any {
	data, err := fixtures.ReadFile("testdata/records/" + name + ".json")
	if err != nil {
		panic(fmt.Sprintf("policytest: no record fixture %s: %v", name, err))
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		panic(fmt.Sprintf("policytest: record fixture %s: %v", name, err))
	}
	return value
}
