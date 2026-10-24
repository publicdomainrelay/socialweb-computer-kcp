package guestagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

const (
	postTimeout = 20 * time.Second

	postBackoffMin = time.Second

	postBackoffMax = 30 * time.Second

	maxReplyBody = 4096
)

var errRefused = errors.New("guestagent: the bidder refused the event")

type publicKeyRecord struct {
	Type string `json:"$type"`

	Kind string `json:"kind"`

	Tag string `json:"tag"`

	Key string `json:"key"`

	CreatedAt string `json:"createdAt"`
}

type onNetworkRecord struct {
	Type string `json:"$type"`

	Kind string `json:"kind"`

	Tag string `json:"tag"`

	Address string `json:"address"`

	CreatedAt string `json:"createdAt"`
}

type signalRecord struct {
	Type string `json:"$type"`

	Kind string `json:"kind"`

	Tag string `json:"tag"`

	Ciphertext string `json:"ciphertext"`

	CreatedAt string `json:"createdAt"`
}

type eventsRequest struct {
	AcceptURI string `json:"acceptUri"`

	AcceptCID string `json:"acceptCid"`

	Record any `json:"record"`
}

type poster struct {
	url string

	accept AcceptRef

	tokens *tokenSource

	client *http.Client

	log *slog.Logger
}

func createdAt() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

func (p *poster) Post(ctx context.Context, nsid, tag string, record any) error {
	body, err := json.Marshal(eventsRequest{AcceptURI: p.accept.URI, AcceptCID: p.accept.CID, Record: record})
	if err != nil {
		return err
	}
	backoff := postBackoffMin
	for attempt := 1; ; attempt++ {
		status, err := p.postOnce(ctx, body)
		switch {
		case err == nil:
			p.log.Info("posted a guest event", "collection", nsid, "tag", tag, "attempts", attempt)
			return nil
		case errors.Is(err, errRefused):
			p.log.Error("the bidder refused a guest event; not retrying", "collection", nsid, "tag", tag, "status", status, "err", err.Error())
			return err
		case ctx.Err() != nil:
			return ctx.Err()
		}
		p.log.Warn("posting a guest event failed; retrying", "collection", nsid, "tag", tag, "status", status, "attempt", attempt, "retryIn", backoff.String(), "err", err.Error())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, postBackoffMax)
	}
}

func (p *poster) postOnce(ctx context.Context, body []byte) (int, error) {
	token, err := p.tokens.Token(ctx)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, postTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, maxReplyBody))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return resp.StatusCode, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		p.tokens.Invalidate(token)
		return resp.StatusCode, fmt.Errorf("the bidder refused the token: %s", errorCode(reply))
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusRequestEntityTooLarge || resp.StatusCode == http.StatusUnprocessableEntity:
		return resp.StatusCode, fmt.Errorf("%w: %s", errRefused, errorCode(reply))
	default:
		return resp.StatusCode, fmt.Errorf("http %d: %s", resp.StatusCode, errorCode(reply))
	}
}

func errorCode(reply []byte) string {
	var body struct {
		Error string `json:"error"`

		Message string `json:"message"`
	}
	if json.Unmarshal(reply, &body) != nil || body.Error == "" {
		return fmt.Sprintf("%d bytes", len(reply))
	}
	if body.Message == "" {
		return body.Error
	}
	return body.Error + ": " + tail(body.Message, 256)
}

func (a *Agent) announce(ctx context.Context, tag string) {
	key := publicKeyRecord{
		Type:      PublicKeyNSID,
		Kind:      publicKeyKindAgeX25519,
		Tag:       tag,
		Key:       a.identity.Recipient().String(),
		CreatedAt: createdAt(),
	}
	if err := a.poster.Post(ctx, key.Type, tag, key); err != nil {
		return
	}
	network := onNetworkRecord{
		Type:      OnNetworkNSID,
		Kind:      onNetworkKindWebRTC,
		Tag:       tag,
		Address:   a.fingerprint,
		CreatedAt: createdAt(),
	}
	_ = a.poster.Post(ctx, network.Type, tag, network)
}
