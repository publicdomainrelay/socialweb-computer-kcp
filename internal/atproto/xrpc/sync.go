package xrpc

import "context"

const RequestCrawlNSID = "com.atproto.sync.requestCrawl"

func RequestCrawl(ctx context.Context, c *Client, hostname string) error {
	_, err := Procedure[map[string]string, struct{}](ctx, c, RequestCrawlNSID, map[string]string{"hostname": hostname})
	return err
}
