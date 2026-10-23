#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

usage() {
  cat >&2 <<EOF
usage: $0

Run it while a rebase (or merge, or cherry-pick) is stopped on conflicts. For every conflicted path
that a generator writes, it takes one side (ours: during a rebase that is the branch being rebased
onto), reruns the generators and stages the result:

  go generate ./api/...    deploy/*-apiresourceschema.yaml, deploy/*-apiexport.yaml,
                           deploy/apiresourceschema-revisions.txt, the TypeScript wire types and
                           fixtures, api/**/zz_generated.register.go and
                           api/**/zz_generated.conditions.go (cmd/socialweb-apigen -list names
                           them all), and api/**/zz_generated.deepcopy.go (deepcopy-gen through
                           hack/codegen.sh)

It never hand-merges generated output. The generators read api/v1alpha1, internal/apigen and
cmd/socialweb-apigen, so a conflict there must be resolved first (the script says so and stops).
It then prints the conflicts that remain (hand-written code, docs, plans) and, when none remain, the
command to continue.
EOF
  exit 2
}
[ $# -eq 0 ] || usage

say() { echo "regen-after-rebase: $*" >&2; }

op=""
gitdir="$(git rev-parse --git-dir)"
if [ -d "$gitdir/rebase-merge" ] || [ -d "$gitdir/rebase-apply" ]; then
  op=rebase
elif [ -f "$gitdir/MERGE_HEAD" ]; then
  op=merge
elif [ -f "$gitdir/CHERRY_PICK_HEAD" ]; then
  op=cherry-pick
fi

mapfile -t conflicted < <(git diff --name-only --diff-filter=U)
if [ ${#conflicted[@]} -eq 0 ]; then
  say "no conflicted paths${op:+ (a $op is in progress)}; regenerating anyway"
fi

inputs=()
codegen=()
for p in "${conflicted[@]}"; do
  case "$p" in
    api/*zz_generated.*.go) codegen+=("$p") ;;
    api/*.go|internal/apigen/*|cmd/socialweb-apigen/*|hack/codegen.sh) inputs+=("$p") ;;
  esac
done
if [ ${#inputs[@]} -gt 0 ]; then
  say "the generators read these conflicted paths; resolve them by hand, git add them, then run this again:"
  printf '  %s\n' "${inputs[@]}"
  exit 1
fi

lockf=deploy/apiresourceschema-revisions.txt
if [ -f "$lockf" ] && [ -n "$(awk '{print $1}' "$lockf" | sort | uniq -d)" ]; then
  tmp="$(mktemp -d)"
  git show "HEAD:$lockf" >"$tmp/head" 2>/dev/null || : >"$tmp/head"
  awk 'NR == FNR { head[$1] = $2; next }
    $1 != "" { n[$1]++; h[$1, n[$1]] = $2; if (!($1 in seen)) { seen[$1] = 1; names[++k] = $1 } }
    END { for (i = 1; i <= k; i++) { nm = names[i]; pick = h[nm, 1]; for (j = 1; j <= n[nm]; j++) if (h[nm, j] == head[nm]) pick = h[nm, j]; print nm, pick } }' \
    "$tmp/head" "$lockf" >"$tmp/lock"
  mv "$tmp/lock" "$lockf"
  rm -rf "$tmp"
  say "$lockf named a revision twice (both sides changed one schema under one revision); kept the HEAD hash. If go generate now asks for a Revision bump, bump it in api/v1alpha1/resources.go and run this again"
fi

for p in "${codegen[@]}"; do
  rm -f -- "$p"
  say "dropped conflicted $p (regenerated below)"
done
find api \( -name zz_generated.register.go -o -name zz_generated.conditions.go \) -delete
say "hack/codegen.sh"
bash hack/codegen.sh || { say "hack/codegen.sh failed (above): fix the api types it names, git add them, run this again"; exit 1; }

before_out="$(go run ./cmd/socialweb-apigen -root . -list)" || { say "cmd/socialweb-apigen failed (above): fix what it names (for example bump Revision in api/v1alpha1/resources.go), git add it, run this again"; exit 1; }
mapfile -t before < <(echo "$before_out" | tr ' ' '\n' | grep .)
generated=()
for p in "${conflicted[@]}"; do
  if printf '%s\n' "${before[@]}" | grep -qxF -- "$p"; then
    generated+=("$p")
  fi
done

for p in "${generated[@]}"; do
  if git cat-file -e ":2:$p" 2>/dev/null; then
    git checkout --ours -- "$p"
  elif git cat-file -e ":3:$p" 2>/dev/null; then
    git checkout --theirs -- "$p"
  else
    git rm -q --cached -- "$p"
    rm -f -- "$p"
  fi
  say "took ours of $p"
done

say "go generate ./api/..."
go generate ./api/... || { say "go generate failed (above): fix what it names (for example bump Revision in api/v1alpha1/resources.go), git add it, run this again"; exit 1; }
mapfile -t outputs < <(go run ./cmd/socialweb-apigen -root . -list | tr ' ' '\n' | grep .)
for p in "${generated[@]}"; do
  if ! printf '%s\n' "${outputs[@]}" | grep -qxF -- "$p"; then
    say "warn: $p was taken from one side but apigen no longer writes it; it stays as taken"
  fi
done

stage=()
for p in "${outputs[@]}"; do
  [ -e "$p" ] && stage+=("$p")
done
git add -- "${stage[@]}"
git add -A -- ':(glob)api/**/zz_generated.*.go'
say "regenerated and staged ${#stage[@]} generated paths (${#generated[@]} were conflicted)"

mapfile -t left < <(git diff --name-only --diff-filter=U)
if [ ${#left[@]} -gt 0 ]; then
  say "conflicts that remain (hand-resolve, git add):"
  printf '  %s\n' "${left[@]}"
  exit 1
fi
case "$op" in
  rebase) say "no conflicts remain: git rebase --continue" ;;
  merge) say "no conflicts remain: git commit" ;;
  cherry-pick) say "no conflicts remain: git cherry-pick --continue" ;;
  *) say "no conflicts remain" ;;
esac
