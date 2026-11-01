package vaultlayout

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	NodesKVMount = "socialweb-nodes"

	NodeJWTPolicyPrefix = "node-"

	NodeJWTMountPrefix = "jwt-"

	NodeJWTMountHashHex = 12

	NodeJWTTokenTTL = "5m"

	NodeJWTUserClaim = "sub"

	NodeJWTRoleType = "jwt"
)

func NodeKVDataPath(node string) string {
	return NodesKVMount + "/data/" + node
}

func NodeKVMetadataPath(node string) string {
	return NodesKVMount + "/metadata/" + node
}

func NodeJWTPolicyName(node string) string {
	return NodeJWTPolicyPrefix + node
}

func NodeFromJWTPolicy(name string) (string, bool) {
	node, ok := strings.CutPrefix(name, NodeJWTPolicyPrefix)
	if !ok || node == "" {
		return "", false
	}
	return node, true
}

func NodeJWTPolicyHCL(node string) string {
	return readPolicy(NodeKVDataPath(node))
}

func NodeJWTRoleName(node string) string {
	return node
}

func NodeJWTMount(issuer string) string {
	sum := sha256.Sum256([]byte(issuer))
	return NodeJWTMountPrefix + hex.EncodeToString(sum[:])[:NodeJWTMountHashHex]
}

func IsNodeJWTMount(path string) bool {
	name := strings.Trim(path, "/")
	rest, ok := strings.CutPrefix(name, NodeJWTMountPrefix)
	if !ok || len(rest) != NodeJWTMountHashHex {
		return false
	}
	_, err := hex.DecodeString(rest)
	return err == nil
}
