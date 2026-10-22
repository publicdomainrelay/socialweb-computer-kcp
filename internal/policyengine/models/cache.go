package models

type CacheFileEncoding string

const (
	CacheEncodingText   CacheFileEncoding = "text"
	CacheEncodingBase64 CacheFileEncoding = "base64"
)

type CacheFile struct {
	Data     string            `json:"data"`
	Encoding CacheFileEncoding `json:"encoding"`
}

type CacheEntry map[string]CacheFile

type Cache map[string]CacheEntry
