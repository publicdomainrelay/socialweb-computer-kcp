package appcli

import (
	"bufio"
	"cmp"
	"context"
	"fmt"
	"io"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

type MarketIdentityClient interface {
	GetMarketIdentity(ctx context.Context, name string) (*v1alpha1.MarketIdentity, error)
	ApproveAssociation(ctx context.Context, name, callerDid string) error
	RejectAssociation(ctx context.Context, name, callerDid string) error
}

type MarketIdentityWatch func(ctx context.Context, name string, each func(*v1alpha1.MarketIdentity) (bool, error)) (*v1alpha1.MarketIdentity, error)

type IdentityRenderer struct {
	Client MarketIdentityClient
	Watch  MarketIdentityWatch
	Out    io.Writer
	In     io.Reader
}

func actionKey(a *v1alpha1.MarketIdentityPendingAction) string {
	if a == nil {
		return ""
	}
	return strings.Join([]string{string(a.Kind), a.URL, a.CallerDid}, "|")
}

func (r IdentityRenderer) WaitReady(ctx context.Context, name string) (*v1alpha1.MarketIdentity, error) {
	if r.Watch == nil {
		return nil, fmt.Errorf("marketidentity %s: the renderer has no watch", name)
	}
	var in *bufio.Reader
	if r.In != nil {
		in = bufio.NewReader(r.In)
	}
	shown := ""
	var failed error
	obj, err := r.Watch(ctx, name, func(obj *v1alpha1.MarketIdentity) (bool, error) {
		if obj == nil {
			return false, fmt.Errorf("marketidentity %s: kcpclient: read marketidentity %s: %w", name, name,
				apierrors.NewNotFound(v1alpha1.MarketIdentityGVR.GroupResource(), name))
		}
		switch obj.Status.Phase {
		case v1alpha1.MarketIdentityPhaseReady:
			fmt.Fprintf(r.Out, "signed in as %s (%s)\n", cmp.Or(obj.Status.Handle), cmp.Or(obj.Status.Did))
			return true, nil
		case v1alpha1.MarketIdentityPhaseFailed:
			failed = fmt.Errorf("marketidentity %s failed: %s", name, obj.Status.Message)
			return true, nil
		}
		if key := actionKey(obj.Status.PendingAction); key != "" && key != shown {
			shown = key
			if err := r.render(ctx, name, obj.Status.PendingAction, in); err != nil {
				return false, err
			}
		}
		return false, nil
	})
	if err != nil {
		return obj, err
	}
	return obj, failed
}

func (r IdentityRenderer) render(ctx context.Context, name string, a *v1alpha1.MarketIdentityPendingAction, in *bufio.Reader) error {
	switch a.Kind {
	case v1alpha1.PendingActionScanQr:
		r.showUrl("Scan this QR code with your phone to sign in:", a)
	case v1alpha1.PendingActionVisitUrl:
		r.showUrl("Open this URL in a browser to sign in:", a)
	case v1alpha1.PendingActionSignInAgain:
		r.showUrl("The sign-in session expired or lacks new scopes. Scan again to sign in:", a)
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
	if a.URL != "" {
		if q, err := qrcode.New(a.URL, qrcode.High); err == nil {
			fmt.Fprint(r.Out, q.ToSmallString(false))
		}
		fmt.Fprintf(r.Out, "\n  Or open this URL:\n  %s\n", a.URL)
	}
	if a.ExpiresAt != "" {
		fmt.Fprintf(r.Out, "  Expires at %s\n", a.ExpiresAt)
	}
	fmt.Fprintf(r.Out, "%s\n\n", rule)
}
