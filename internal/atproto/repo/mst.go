package repo

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/bluesky-social/indigo/atproto/repo/mst"
	blocks "github.com/ipfs/go-block-format"
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

func mstPathStep(blk blocks.Block, key []byte) (*cid.Cid, *cid.Cid, error) {
	nd, err := mst.NodeDataFromCBOR(bytes.NewReader(blk.RawData()))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: mst node %s: %v", ErrCorruptRepo, blk.Cid(), err)
	}
	c := blk.Cid()
	n, err := nd.Node(&c)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: mst node %s: %v", ErrCorruptRepo, c, err)
	}
	var child *cid.Cid
	for _, e := range n.Entries {
		switch {
		case e.ChildCID != nil:
			child = e.ChildCID
		case bytes.Equal(key, e.Key):
			return nil, e.Value, nil
		case bytes.Compare(key, e.Key) < 0:
			return child, nil, nil
		default:
			child = nil
		}
	}
	return child, nil, nil
}
