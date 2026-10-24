#!/usr/bin/env bash
set -euo pipefail

image="gitlab/gitlab-ce:19.2.7-ce.0@sha256:a5d347f51bf2d619375d9c075fd40d38effacbf03615cf145810bda151de9c64"
ready_wait_s=300
runner_tag=socialweb-computer-fixture

usage() {
  cat >&2 <<'EOF'
usage: scripts/gitlab-fixture.sh up [--name NAME] [--address ADDR]
       scripts/gitlab-fixture.sh down [--name NAME]
  up    starts a fresh GitLab CE fixture (pinned image, its own docker volumes, plain http on ADDR and a
        port docker picks), sets a generated root password, creates the project root/fixture-probe from
        deploy/gitlab-fixture/template/ and a project runner tagged socialweb-computer-fixture, and prints
        the fixture's env file (URL, project, runner id, paths of the 0600 secret files; never a secret)
  down  removes the container, its volumes and its state directory; refuses nothing, leaves nothing
  --name     the fixture instance (default default); state in ~/.tmp/gitlab-fixture/<name>
  --address  the address the fixture publishes on, reachable by the runner (default: this host's source
             address of the default route)
EOF
  exit 2
}

[ $# -ge 1 ] || usage
cmd="$1"
shift
name=default
address=""
while [ $# -gt 0 ]; do
  case "$1" in
    --name) name="$2"; shift ;;
    --address) address="$2"; shift ;;
    *) usage ;;
  esac
  shift
done
case "$name" in *[!a-z0-9-]* | "") echo "gitlab-fixture: --name must match [a-z0-9-]+" >&2; exit 2 ;; esac

die() { echo "gitlab-fixture: $*" >&2; exit 1; }
say() { echo "gitlab-fixture: $(date -u +%H:%M:%S) $*" >&2; }

for tool in docker jq curl base64; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required"
done

root="$(cd "$(dirname "$0")/.." && pwd)"
template="$root/deploy/gitlab-fixture/template"
label="socialweb.gitlab-fixture=$name"
container="socialweb-gitlab-fixture-$name"
state="$HOME/.tmp/gitlab-fixture/$name"
secrets="$state/secrets"
envfile="$state/fixture.env"

down() {
  local ids vols
  ids="$(docker ps -aq --filter "label=$label")"
  [ -z "$ids" ] || docker rm -f $ids >/dev/null
  vols="$(docker volume ls -q --filter "label=$label")"
  [ -z "$vols" ] || docker volume rm $vols >/dev/null
  rm -rf "$state"
  [ -z "$(docker ps -aq --filter "label=$label")" ] || die "a container labeled $label is still there"
  [ -z "$(docker volume ls -q --filter "label=$label")" ] || die "a volume labeled $label is still there"
  say "fixture $name is down; nothing labeled $label is left and $state is gone"
}

if [ "$cmd" = down ]; then
  down
  exit 0
fi
[ "$cmd" = up ] || usage

[ -z "$(docker ps -aq --filter "label=$label")" ] || die "fixture $name exists; run: scripts/gitlab-fixture.sh down --name $name"
if [ -z "$address" ]; then
  address="$(ip -4 route get 1.1.1.1 | sed -n 's/.* src \([0-9.]*\).*/\1/p')"
  [ -n "$address" ] || die "no default route source address; pass --address"
fi

rm -rf "$state"
mkdir -p "$secrets"
chmod 0700 "$state" "$secrets"
random() { head -c 32 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | cut -c1-"$1"; }
(umask 077; random 32 >"$secrets/root-password"; printf 'glpat-%s' "$(random 26)" >"$secrets/api-token")

for vol in config logs data; do
  docker volume create --label "$label" "$container-$vol" >/dev/null
done

omnibus="$(printf '%s\n' \
  "f = '/etc/gitlab/fixture_external_url'" \
  "100.times { break if File.exist?(f); sleep 0.1 }" \
  "external_url File.read(f).strip" \
  "nginx['listen_port'] = 80" \
  "nginx['listen_https'] = false" \
  "puma['worker_processes'] = 0" \
  "sidekiq['concurrency'] = 5" \
  "prometheus_monitoring['enable'] = false" \
  "gitlab_kas['enable'] = false" \
  "registry['enable'] = false" \
  "gitlab_rails['usage_ping_enabled'] = false")"

say "starting $container from $image"
docker run -d --name "$container" --label "$label" --hostname "gitlab-fixture-$name" \
  --shm-size 256m --memory 8g \
  -p "$address::80" \
  -v "$container-config:/etc/gitlab" -v "$container-logs:/var/log/gitlab" -v "$container-data:/var/opt/gitlab" \
  -e GITLAB_OMNIBUS_CONFIG="$omnibus" \
  "$image" >/dev/null
port="$(docker port "$container" 80/tcp | awk 'NR==1' | sed 's/.*://')"
[ -n "$port" ] || { down; die "docker published no port for 80/tcp"; }
url="http://$address:$port"
printf '%s\n' "$url" | docker exec -i "$container" sh -c 'cat >/etc/gitlab/fixture_external_url'
say "$container publishes $url; waiting for GitLab (at most ${ready_wait_s}s)"

started="$(date +%s)"
until curl -fsS -o /dev/null --max-time 5 "$url/users/sign_in" 2>/dev/null; do
  if [ "$(docker inspect -f '{{.State.Running}}' "$container")" != true ]; then
    docker logs --tail 40 "$container" >&2 || true
    down
    die "the GitLab container stopped during its first start"
  fi
  if [ $(( $(date +%s) - started )) -ge "$ready_wait_s" ]; then
    docker logs --tail 40 "$container" >&2 || true
    down
    die "GitLab did not answer $url/users/sign_in within ${ready_wait_s}s"
  fi
  sleep 3
done
say "GitLab answers after $(( $(date +%s) - started ))s"

rails='lines = STDIN.read.split("\n")
u = User.find_by_username("root")
u.password = lines[0]
u.password_confirmation = lines[0]
u.password_automatically_set = false
u.save!
t = u.personal_access_tokens.create!(scopes: ["api"], name: "gitlab-fixture", expires_at: 2.days.from_now)
t.set_token(lines[1])
t.save!'
{ cat "$secrets/root-password"; echo; cat "$secrets/api-token"; echo; } |
  timeout -k 5 240 docker exec -i "$container" gitlab-rails runner "$rails" >&2 ||
  { down; die "setting the root password and the API token failed"; }
say "root password and API token set (files in $secrets, mode 0600)"

api_token="$(cat "$secrets/api-token")"
api() {
  local method="$1" path="$2"
  shift 2
  printf 'header = "PRIVATE-TOKEN: %s"\n' "$api_token" |
    curl -sS --max-time 30 --fail-with-body -K - -X "$method" "$url/api/v4$path" "$@"
}

project_id="$(api POST /projects --data-urlencode name=fixture-probe --data-urlencode path=fixture-probe \
  --data-urlencode visibility=private | jq -er .id)"
actions='[]'
while IFS= read -r rel; do
  actions="$(printf '%s' "$actions" | jq -c --arg p "$rel" --rawfile c "$template/$rel" '. + [{action: "create", file_path: $p, content: $c}]')"
done < <(cd "$template" && find . -type f -printf '%P\n' | sort)
commit_body="$(jq -nc --argjson a "$actions" '{branch: "main", commit_message: "fixture template from socialweb-computer-kcp deploy/gitlab-fixture/template", actions: $a}')"
commit_sha="$(api POST "/projects/$project_id/repository/commits" -H 'content-type: application/json' --data "$commit_body" | jq -er .id)"
say "project root/fixture-probe ($project_id) has the template at $commit_sha"

resp="$(api POST /user/runners --data-urlencode runner_type=project_type --data-urlencode "project_id=$project_id" \
  --data-urlencode "tag_list=$runner_tag" --data-urlencode run_untagged=false --data-urlencode locked=true \
  --data-urlencode "description=socialweb-computer fixture runner ($name)")"
runner_id="$(printf '%s' "$resp" | jq -er .id)"
(umask 077; printf '%s' "$resp" | jq -er .token >"$secrets/runner-token")
unset resp

pipeline_id=""
for _ in $(seq 1 20); do
  pipeline_id="$(api GET "/projects/$project_id/pipelines?sha=$commit_sha" | jq -r '.[0].id // empty')"
  [ -n "$pipeline_id" ] && break
  sleep 1
done
[ -n "$pipeline_id" ] || { down; die "the template commit $commit_sha made no pipeline"; }

(umask 077; printf '%s\n' \
  "GITLAB_FIXTURE_NAME=$name" \
  "GITLAB_FIXTURE_URL=$url" \
  "GITLAB_FIXTURE_CONTAINER=$container" \
  "GITLAB_FIXTURE_PROJECT_ID=$project_id" \
  "GITLAB_FIXTURE_PROJECT_PATH=root/fixture-probe" \
  "GITLAB_FIXTURE_PIPELINE_ID=$pipeline_id" \
  "GITLAB_FIXTURE_RUNNER_ID=$runner_id" \
  "GITLAB_FIXTURE_RUNNER_TAG=$runner_tag" \
  "GITLAB_FIXTURE_RUNNER_TOKEN_FILE=$secrets/runner-token" \
  "GITLAB_FIXTURE_API_TOKEN_FILE=$secrets/api-token" \
  "GITLAB_FIXTURE_ROOT_PASSWORD_FILE=$secrets/root-password" >"$envfile")
say "fixture $name is up; pipeline $pipeline_id waits for a runner tagged $runner_tag; env file $envfile"
cat "$envfile"
