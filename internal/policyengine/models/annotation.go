package models

type AnnotationLevel string

const (
	AnnotationError   AnnotationLevel = "error"
	AnnotationWarning AnnotationLevel = "warning"
	AnnotationNotice  AnnotationLevel = "notice"
)

type Annotation struct {
	Path            string          `json:"path,omitempty"`
	AnnotationLevel AnnotationLevel `json:"annotation_level,omitempty"`
	Title           string          `json:"title,omitempty"`
	Message         string          `json:"message,omitempty"`
	RawDetails      string          `json:"raw_details,omitempty"`
	StartLine       int             `json:"start_line,omitempty"`
	EndLine         int             `json:"end_line,omitempty"`
}

type Annotations map[string]any
