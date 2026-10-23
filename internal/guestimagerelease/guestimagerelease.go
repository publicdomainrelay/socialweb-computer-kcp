package guestimagerelease

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestimage"
)

const (
	MaxManifest   = 64 << 10
	ClientTimeout = 30 * time.Second
)

type Options struct {
	Base           string
	Target         string
	Version        string
	Commit         string
	ManifestSHA256 string
	Client         *http.Client
}

const LatestRef = "main/latest"

var (
	ErrNotPublished = errors.New("guestimagerelease: no published guest image")
	errUnreadable   = errors.New("guestimagerelease: a candidate could not be read")
	ErrPinMismatch  = errors.New("guestimagerelease: every published guest image differs from this build's pin")
)

func Source(ctx context.Context, o Options) (string, error) {
	if o.Base == "" {
		return "", errors.New("guestimagerelease: no base to download a guest image from")
	}
	if o.Target == "" {
		return "", errors.New("guestimagerelease: this host has no guest image target")
	}
	if o.ManifestSHA256 == "" {
		return "", errors.New("guestimagerelease: no pinned sha256 for the manifest, so a candidate cannot be matched against this build; pass the pin in versions.json")
	}
	base := strings.TrimSuffix(o.Base, "/")
	client := o.Client
	if client == nil {
		client = &http.Client{Timeout: ClientTimeout}
	}
	refs := make([]string, 0, 4)
	add := func(ref string) {
		if slices.Contains(refs, ref) {
			return
		}
		refs = append(refs, ref)
	}
	if o.Version != "" {
		add("releases/" + o.Version)
	}
	if o.Commit != "" {
		add("main/" + o.Commit)
	}
	add(LatestRef)
	tried := make([]string, 0, len(refs)+1)
	mismatched := make([]string, 0, len(refs)+1)
	unreadable := make([]string, 0, len(refs)+1)
	try := func(ref string) (string, bool, error) {
		src := base + "/" + ref + "/guest-image/" + o.Target
		manifest := src + "/" + guestimage.ManifestName
		ok, err := published(ctx, client, manifest)
		if err != nil {
			if errors.Is(err, errUnreadable) {
				unreadable = append(unreadable, src)
				return src, false, nil
			}
			return "", false, err
		}
		if !ok {
			tried = append(tried, src)
			return src, false, nil
		}
		match, err := manifestMatches(ctx, client, manifest, o.ManifestSHA256)
		if err != nil {
			if errors.Is(err, errUnreadable) {
				unreadable = append(unreadable, src)
				return src, false, nil
			}
			return "", false, err
		}
		if !match {
			mismatched = append(mismatched, src)
			return src, false, nil
		}
		return src, true, nil
	}
	for _, ref := range refs {
		src, matched, err := try(ref)
		if err != nil {
			return "", err
		}
		if matched {
			return src, nil
		}
	}
	newest, listErr := newestRelease(ctx, client, base)
	if listErr == nil && newest != "" {
		if ref := "releases/" + newest; !slices.Contains(refs, ref) {
			src, matched, err := try(ref)
			if err != nil {
				return "", err
			}
			if matched {
				return src, nil
			}
		}
	}
	looked := ""
	if len(tried) > 0 {
		looked = ", tried " + strings.Join(tried, ", ")
	}
	unread := ""
	if len(unreadable) > 0 {
		unread = ", and these answered that they could not be read: " + strings.Join(unreadable, ", ")
	}
	absent := ""
	if len(tried) > 0 {
		absent = "; absent at " + strings.Join(tried, ", ")
	}
	unlisted := ""
	if listErr != nil {
		unlisted = fmt.Sprintf(", and the list of releases at %s could not be read: %v", base, listErr)
	}
	if len(mismatched) > 0 {
		return "", fmt.Errorf("%w (%s): published but different at %s%s%s%s", ErrPinMismatch, o.ManifestSHA256,
			strings.Join(mismatched, ", "), absent, unread, unlisted)
	}
	if len(unreadable) > 0 {
		return "", fmt.Errorf("%w for target %s%s%s%s", ErrNotPublished, o.Target, looked, unread, unlisted)
	}
	if listErr != nil {
		return "", fmt.Errorf("%w for target %s%s, and the list of releases at %s could not be read: %w",
			ErrNotPublished, o.Target, looked, base, listErr)
	}
	return "", fmt.Errorf("%w for target %s%s", ErrNotPublished, o.Target, looked)
}

func manifestMatches(ctx context.Context, client *http.Client, url, want string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("guestimagerelease: %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("%w: %s answered %s", errUnreadable, url, resp.Status)
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(resp.Body, MaxManifest)); err != nil {
		return false, fmt.Errorf("guestimagerelease: reading %s: %w", url, err)
	}
	return hex.EncodeToString(h.Sum(nil)) == want, nil
}

func newestRelease(ctx context.Context, client *http.Client, base string) (string, error) {
	url := base + "/?list-type=2&prefix=releases/&delimiter=/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("guestimagerelease: %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("guestimagerelease: %s answered %s", url, resp.Status)
	}
	var list struct {
		Common []struct {
			Prefix string `xml:"Prefix"`
		} `xml:"CommonPrefixes"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&list); err != nil {
		return "", fmt.Errorf("guestimagerelease: reading the listing of %s: %w", base, err)
	}
	newest := ""
	for _, p := range list.Common {
		v := strings.TrimSuffix(strings.TrimPrefix(p.Prefix, "releases/"), "/")
		if !strings.HasPrefix(v, "v") {
			continue
		}
		if newest == "" || compareVersion(v, newest) > 0 {
			newest = v
		}
	}
	return newest, nil
}

func compareVersion(a, b string) int {
	an, ap := splitVersion(a)
	bn, bp := splitVersion(b)
	for i := range 3 {
		if an[i] != bn[i] {
			return sign(an[i] - bn[i])
		}
	}
	switch {
	case ap == "" && bp == "":
		return 0
	case ap == "":
		return 1
	case bp == "":
		return -1
	}
	pa, na := splitPre(ap)
	pb, nb := splitPre(bp)
	if pa != pb {
		if pa < pb {
			return -1
		}
		return 1
	}
	return sign(na - nb)
}

func splitVersion(v string) ([3]int, string) {
	v = strings.TrimPrefix(v, "v")
	pre := ""
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v, pre = v[:i], v[i+1:]
	}
	var out [3]int
	for i, part := range strings.SplitN(v, ".", 3) {
		n, err := strconv.Atoi(part)
		if err == nil {
			out[i] = n
		}
	}
	return out, pre
}

func splitPre(s string) (string, int) {
	i := 0
	for i < len(s) && (s[i] < '0' || s[i] > '9') {
		i++
	}
	n, _ := strconv.Atoi(s[i:])
	return s[:i], n
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

func published(ctx context.Context, client *http.Client, url string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("guestimagerelease: %s: %w", url, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	case http.StatusForbidden:
		return false, fmt.Errorf("%w: %s answered %s, so the object may be present but unreadable", errUnreadable, url, resp.Status)
	}
	return false, fmt.Errorf("%w: %s answered %s", errUnreadable, url, resp.Status)
}
