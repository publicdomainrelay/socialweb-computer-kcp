package baosync

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
)

type Report struct {
	Applied   []Op
	Conflicts []string
	Err       error
}

func (r Report) Changed() bool { return len(r.Applied) > 0 }

func (r Report) Summary() string {
	if len(r.Applied) == 0 && len(r.Conflicts) == 0 && r.Err == nil {
		return "in sync"
	}
	var b strings.Builder
	counts := map[OpKind]int{}
	var order []OpKind
	for _, o := range r.Applied {
		if counts[o.Kind] == 0 {
			order = append(order, o.Kind)
		}
		counts[o.Kind]++
	}
	for i, k := range order {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s x%d", k, counts[k])
	}
	if len(r.Conflicts) > 0 {
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%d conflict(s): %s", len(r.Conflicts), strings.Join(r.Conflicts, "; "))
	}
	if r.Err != nil {
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString("errors: " + r.Err.Error())
	}
	return b.String()
}

func enableSecret(ctx context.Context, c *openbao.Client, o Op) error {
	var err error
	switch o.Type {
	case MountKV2:
		err = c.EnableKV(ctx, o.Target, 2)
	case MountKV:
		err = c.EnableKV(ctx, o.Target, 1)
	case MountPKI:
		err = c.MountPKI(ctx, o.Target)
	default:
		err = fmt.Errorf("baosync: no way to enable a %s mount", o.Type)
	}
	if err == nil && len(o.Body) > 0 {
		err = c.TuneMount(ctx, o.Target, o.Body)
	}
	return err
}

func applyOp(ctx context.Context, c *openbao.Client, o Op) error {
	switch o.Kind {
	case OpEnableSecret:
		return enableSecret(ctx, c, o)
	case OpEnableAuth:
		return c.EnableAuth(ctx, o.Target, o.Type)
	case OpDisableAuth:
		return c.DisableAuth(ctx, o.Target)
	case OpPutPolicy:
		return c.PutPolicy(ctx, o.Target, o.HCL)
	case OpWriteObject:
		return c.Write(ctx, o.Target, o.Body)
	case OpDeleteObject, OpDeleteKV:
		return c.Delete(ctx, o.Target)
	case OpDeletePolicy:
		return c.DeletePolicy(ctx, o.Target)
	case OpDisableSecret:
		return c.DisableMount(ctx, o.Target)
	case OpRegister:
		return c.Write(ctx, "sys/plugins/catalog/"+o.Target, o.Body)
	case OpReload:
		return c.Write(ctx, "sys/plugins/reload/backend", o.Body)
	case OpDeregister:
		return c.Delete(ctx, "sys/plugins/catalog/"+o.Target)
	}
	return fmt.Errorf("baosync: unknown op %s", o.Kind)
}

func Apply(ctx context.Context, c *openbao.Client, p Plan) Report {
	r := Report{Conflicts: p.Conflicts}
	var errs []error
	for _, o := range p.Ops {
		if err := applyOp(ctx, c, o); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", o, err))
			continue
		}
		r.Applied = append(r.Applied, o)
	}
	r.Err = errors.Join(errs...)
	return r
}

func Sync(ctx context.Context, c *openbao.Client, d Desired) (Report, error) {
	if err := d.Validate(); err != nil {
		return Report{}, err
	}
	a, err := Read(ctx, c, d)
	if err != nil {
		return Report{}, err
	}
	return Apply(ctx, c, Diff(d, a)), nil
}
