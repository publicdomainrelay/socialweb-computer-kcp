package appcli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

type MarketIdentityClient interface {
	GetMarketIdentity(ctx context.Context, name string) (*v1alpha1.MarketIdentity, error)
	ApproveAssociation(ctx context.Context, name, callerDid string) error
	RejectAssociation(ctx context.Context, name, callerDid string) error
}

type IdentityRenderer struct {
	Client   MarketIdentityClient
	Out      io.Writer
	In       io.Reader
	Interval time.Duration
}

const defaultIdentityInterval = time.Second

func actionKey(a *v1alpha1.MarketIdentityPendingAction) string {
	if a == nil {
		return ""
	}
	return strings.Join([]string{string(a.Kind), a.Url, a.CallerDid}, "|")
}

func (r IdentityRenderer) WaitReady(ctx context.Context, name string) (*v1alpha1.MarketIdentity, error) {
	interval := r.Interval
	if interval <= 0 {
		interval = defaultIdentityInterval
	}
	var in *bufio.Reader
	if r.In != nil {
		in = bufio.NewReader(r.In)
	}
	shown := ""
	for {
		obj, err := r.Client.GetMarketIdentity(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("marketidentity %s: %w", name, err)
		}
		switch obj.Status.Phase {
		case v1alpha1.MarketIdentityPhaseReady:
			fmt.Fprintf(r.Out, "signed in as %s (%s)\n", orDash(obj.Status.Handle), orDash(obj.Status.Did))
			return obj, nil
		case v1alpha1.MarketIdentityPhaseFailed:
			return obj, fmt.Errorf("marketidentity %s failed: %s", name, obj.Status.Message)
		}
		if key := actionKey(obj.Status.PendingAction); key != "" && key != shown {
			shown = key
			if err := r.render(ctx, name, obj.Status.PendingAction, in); err != nil {
				return obj, err
			}
		}
		select {
		case <-ctx.Done():
			return obj, ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (r IdentityRenderer) render(ctx context.Context, name string, a *v1alpha1.MarketIdentityPendingAction, in *bufio.Reader) error {
	switch a.Kind {
	case v1alpha1.PendingActionScanQr:
		r.showUrl("Scan this QR code with your phone to sign in:", a)
	case v1alpha1.PendingActionVisitUrl:
		r.showUrl("Open this URL in a browser to sign in:", a)
	case v1alpha1.PendingActionSignInAgain:
		r.showUrl("The sign-in session expired. Scan again to sign in:", a)
	case v1alpha1.PendingActionApproveAssociation:
		who := a.CallerDid
		if a.Handle != "" && a.Handle != a.CallerDid {
			who = fmt.Sprintf("%s (%s)", a.Handle, a.CallerDid)
		}
		if in == nil {
			fmt.Fprintf(r.Out, "%s asks to associate; the accepted path is the operator's verified badgeBlueKeys claim naming %s with the role service; adding the caller to spec.approvedAssociations is a manual workaround only\n", who, a.CallerDid)
			return nil
		}
		fmt.Fprintf(r.Out, "Associate with %s? [y/N] ", who)
		line, err := in.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y") {
			return r.Client.ApproveAssociation(ctx, name, a.CallerDid)
		}
		return r.Client.RejectAssociation(ctx, name, a.CallerDid)
	default:
		fmt.Fprintf(r.Out, "marketidentity %s waits on %s\n", name, a.Kind)
	}
	return nil
}

func (r IdentityRenderer) showUrl(title string, a *v1alpha1.MarketIdentityPendingAction) {
	rule := strings.Repeat("=", 60)
	fmt.Fprintf(r.Out, "\n%s\n  %s\n\n", rule, title)
	if a.Url != "" {
		if q, err := qrcode.New(a.Url, qrcode.High); err == nil {
			fmt.Fprint(r.Out, q.ToSmallString(false))
		}
		fmt.Fprintf(r.Out, "\n  Or open this URL:\n  %s\n", a.Url)
	}
	if a.ExpiresAt != "" {
		fmt.Fprintf(r.Out, "  Expires at %s\n", a.ExpiresAt)
	}
	fmt.Fprintf(r.Out, "%s\n\n", rule)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
