package repo

import (
	"context"
	"fmt"
	"sort"

	"github.com/bluesky-social/indigo/atproto/repo/mst"
	"github.com/ipfs/go-cid"
)

func loadTree(ctx context.Context, bs mst.MSTBlockSource, root cid.Cid) (*mst.Tree, error) {
	tree, err := mst.LoadTreeFromStore(ctx, bs, root)
	if err != nil {
		return nil, fmt.Errorf("loading mst %s: %w", root, err)
	}
	return tree, nil
}

func loadFullTree(ctx context.Context, bs mst.MSTBlockSource, root cid.Cid) (*mst.Tree, error) {
	tree, err := loadTree(ctx, bs, root)
	if err != nil {
		return nil, err
	}
	if tree.IsPartial() {
		return nil, fmt.Errorf("%w: mst %s is partial", ErrCorruptRepo, root)
	}
	return tree, nil
}

func DiffTrees(oldTree, newTree *mst.Tree) ([]CommitOp, error) {
	if newTree == nil {
		return nil, fmt.Errorf("%w: missing new tree", ErrInvalidRequest)
	}
	oldMap := make(map[string]cid.Cid)
	newMap := make(map[string]cid.Cid)
	if oldTree != nil {
		if err := oldTree.WriteToMap(oldMap); err != nil {
			return nil, fmt.Errorf("reading old mst: %w", err)
		}
	}
	if err := newTree.WriteToMap(newMap); err != nil {
		return nil, fmt.Errorf("reading new mst: %w", err)
	}
	keys := make([]string, 0, len(oldMap)+len(newMap))
	for k := range oldMap {
		keys = append(keys, k)
	}
	for k := range newMap {
		if _, ok := oldMap[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	ops := make([]CommitOp, 0, len(keys))
	for _, k := range keys {
		oldCID, inOld := oldMap[k]
		newCID, inNew := newMap[k]
		switch {
		case !inOld && inNew:
			c := newCID
			ops = append(ops, CommitOp{Action: ActionCreate, Path: k, CID: &c})
		case inOld && !inNew:
			p := oldCID
			ops = append(ops, CommitOp{Action: ActionDelete, Path: k, Prev: &p})
		case inOld && inNew && oldCID != newCID:
			c := newCID
			p := oldCID
			ops = append(ops, CommitOp{Action: ActionUpdate, Path: k, CID: &c, Prev: &p})
		}
	}
	return ops, nil
}

func collectNodeCIDs(n *mst.Node, out map[cid.Cid]bool) {
	if n == nil {
		return
	}
	if n.CID != nil {
		out[*n.CID] = true
	}
	for _, e := range n.Entries {
		if e.Child != nil {
			collectNodeCIDs(e.Child, out)
		} else if e.ChildCID != nil {
			out[*e.ChildCID] = true
		}
	}
}
