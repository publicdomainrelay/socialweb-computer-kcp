package v1alpha1

type PolicyCacheEncoding string

const (
	PolicyCacheEncodingText PolicyCacheEncoding = "text"

	PolicyCacheEncodingBase64 PolicyCacheEncoding = "base64"
)

type PolicyCacheFile struct {
	Data string `json:"data"`

	Encoding PolicyCacheEncoding `json:"encoding"`
}

type PolicyCacheEntry map[string]PolicyCacheFile

type PolicyCache map[string]PolicyCacheEntry
