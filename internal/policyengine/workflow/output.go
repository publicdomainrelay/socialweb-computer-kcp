package workflow

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/models"
)

func parseGitHubActionsOutputs(content string) map[string]string {
	outputs := make(map[string]string)
	currentKey := ""
	currentDelimiter := ""
	currentValue := strings.Builder{}
	for _, line := range strings.Split(content, "\n") {
		if currentDelimiter != "" {
			if strings.HasPrefix(line, currentDelimiter) {
				outputs[currentKey] = strings.TrimSuffix(currentValue.String(), "\n")
				currentKey = ""
				currentDelimiter = ""
				currentValue.Reset()
			} else {
				currentValue.WriteString(line + "\n")
			}
			continue
		}
		if currentKey == "" && strings.Contains(line, "<<") {
			parts := strings.SplitN(line, "<<", 2)
			currentKey = strings.TrimSpace(parts[0])
			currentDelimiter = strings.TrimSpace(parts[1])
			continue
		}
		if currentKey == "" && strings.Contains(line, "=") {
			parts := strings.SplitN(line, "=", 2)
			key := strings.TrimSpace(parts[0])
			value := ""
			if len(parts) > 1 {
				value = parts[1]
			}
			outputs[key] = value
		}
	}
	return outputs
}

func (e *Executor) parseAnnotations(output string) {
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, "::") {
			continue
		}
		line = strings.TrimPrefix(line, "::")
		parts := strings.SplitN(line, "::", 2)
		levelAndParams := parts[0]
		message := ""
		if len(parts) > 1 {
			message = parts[1]
		}
		levelParts := strings.SplitN(levelAndParams, " ", 2)
		level := levelParts[0]
		if level != "error" && level != "warning" && level != "notice" {
			continue
		}
		annotation := models.Annotation{
			AnnotationLevel: models.AnnotationLevel(level),
			Message:         message,
			Title:           message,
			RawDetails:      line,
		}
		if len(levelParts) > 1 {
			params, _ := url.ParseQuery(strings.ReplaceAll(levelParts[1], ",", "&"))
			if file := params.Get("file"); file != "" {
				annotation.Path = file
			}
			if path := params.Get("path"); path != "" {
				annotation.Path = path
			}
			if title := params.Get("title"); title != "" {
				annotation.Title = title
			}
			if lineValue := params.Get("line"); lineValue != "" {
				if number, err := strconv.Atoi(lineValue); err == nil {
					annotation.StartLine = number
					annotation.EndLine = number
				}
			}
			if endLine := params.Get("endLine"); endLine != "" {
				if number, err := strconv.Atoi(endLine); err == nil {
					annotation.EndLine = number
				}
			}
		}
		e.Context.Annotations[level] = append(e.Context.Annotations[level], annotation)
	}
}
