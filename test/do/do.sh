#!/usr/bin/env bash
# DigitalOcean droplets for live testing, driven straight off the v2 API.
#
#   test/do/do.sh up   <prefix> <count> [size]   create droplets, wait, print IPs
#   test/do/do.sh ips  <prefix>                  the IPs again
#   test/do/do.sh ssh  <prefix> <n> [cmd]        ssh to the nth droplet
#   test/do/do.sh cut  <prefix>                  block outbound internet (airgap)
#   test/do/do.sh mend <prefix>                  restore outbound internet
#   test/do/do.sh down <prefix>                  destroy droplets and firewalls
#   test/do/do.sh list                           everything this tool created
#
# The token comes from .env.production, which is gitignored and stays out of
# every command line here — an argument is visible in `ps` to any other user
# on the machine, and would end up in shell history.
#
# Everything is tagged k3helper-test so `down` can never touch a droplet this
# script did not create, and `list` can show what is still costing money.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
STATE="$ROOT/test/do/.state"
KEY="$STATE/id_ed25519"
TAG=k3helper-test
REGION="${DO_REGION:-sgp1}"
IMAGE="${DO_IMAGE:-ubuntu-24-04-x64}"
SIZE_DEFAULT="${DO_SIZE:-s-2vcpu-4gb}"

mkdir -p "$STATE"
chmod 700 "$STATE"

if [ -z "${DO_API_KEY:-}" ]; then
  [ -f "$ROOT/.env.production" ] || { echo "error: no .env.production and DO_API_KEY unset" >&2; exit 1; }
  set -a; . "$ROOT/.env.production"; set +a
fi
[ -n "${DO_API_KEY:-}" ] || { echo "error: DO_API_KEY is empty" >&2; exit 1; }

api() {
  local method=$1 path=$2; shift 2
  curl -sS -X "$method" \
    -H "Authorization: Bearer $DO_API_KEY" \
    -H "Content-Type: application/json" \
    "https://api.digitalocean.com/v2$path" "$@"
}

# jq is not assumed; python3 is already required by the sandbox tooling.
pyq() { python3 -c "$1"; }

log() { printf '%s\n' "$*" >&2; }

# --- ssh key -----------------------------------------------------------------
# A throwaway key per checkout, never the developer's own: these droplets are
# public and short-lived, and the key ends up in a state directory.
ensure_key() {
  if [ ! -f "$KEY" ]; then
    ssh-keygen -q -t ed25519 -f "$KEY" -N '' -C k3helper-test
    log "generated $KEY"
  fi
  local fp
  fp=$(ssh-keygen -lf "$KEY.pub" | awk '{print $2}' | sed 's/^SHA256://')
  local id
  id=$(api GET "/account/keys?per_page=200" | pyq "
import json,sys
ks=json.load(sys.stdin)['ssh_keys']
print(next((str(k['id']) for k in ks if k['name']=='k3helper-test'), ''))
")
  if [ -z "$id" ]; then
    # The payload goes through a file rather than a nested command
    # substitution: a public key contains spaces, and quoting it through two
    # levels of shell and one of python is how it silently arrived empty —
    # which produced droplets with no key on them and no way in.
    KEY_PUB="$KEY.pub" python3 -c "
import json,os
print(json.dumps({'name':'k3helper-test','public_key':open(os.environ['KEY_PUB']).read().strip()}))
" > "$STATE/key.json"
    id=$(api POST "/account/keys" -d "@$STATE/key.json" | pyq "
import json,sys
d=json.load(sys.stdin)
k=d.get('ssh_key')
if not k: raise SystemExit('ssh key upload failed: ' + json.dumps(d))
print(k['id'])
")
    log "uploaded ssh key to DigitalOcean (id $id)"
  fi
  [ -n "$id" ] || { log "error: no ssh key id; refusing to create droplets nobody can log into"; exit 1; }
  echo "$id"
}

SSH_OPTS=(-i "$KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null
          -o LogLevel=ERROR -o ConnectTimeout=10 -o BatchMode=yes)

# --- lifecycle ---------------------------------------------------------------
cmd_up() {
  local prefix=$1 count=${2:-2} size=${3:-$SIZE_DEFAULT}
  local keyid; keyid=$(ensure_key)
  local names=()
  for i in $(seq 1 "$count"); do names+=("\"$prefix-$i\""); done

  log "creating $count x $size in $REGION ($IMAGE)..."
  api POST "/droplets" -d "{
    \"names\": [$(IFS=,; echo "${names[*]}")],
    \"region\": \"$REGION\",
    \"size\": \"$size\",
    \"image\": \"$IMAGE\",
    \"ssh_keys\": [$keyid],
    \"tags\": [\"$TAG\", \"$prefix\"]
  }" > "$STATE/$prefix.create.json"

  if grep -q '"id":"' "$STATE/$prefix.create.json" 2>/dev/null; then
    log "create failed:"; cat "$STATE/$prefix.create.json" >&2; exit 1
  fi

  log "waiting for droplets to come up..."
  local tries=0
  while [ $tries -lt 60 ]; do
    api GET "/droplets?tag_name=$prefix&per_page=200" > "$STATE/$prefix.json"
    local ready
    ready=$(pyq "
import json
ds=json.load(open('$STATE/$prefix.json'))['droplets']
up=[d for d in ds if d['status']=='active' and any(n['type']=='public' for n in d['networks']['v4'])]
print(len(up))
")
    [ "$ready" = "$count" ] && break
    sleep 5; tries=$((tries+1))
  done

  log "waiting for sshd..."
  for ip in $(cmd_ips "$prefix"); do
    local t=0
    until ssh "${SSH_OPTS[@]}" "root@$ip" true 2>/dev/null; do
      t=$((t+1)); [ $t -gt 40 ] && { log "error: $ip never accepted ssh"; exit 1; }
      sleep 5
    done
  done

  # sshd answers well before the machine is finished with itself. cloud-init is
  # still running apt, and on a fresh Ubuntu image that includes replacing
  # ca-certificates — during which every https download on the box fails TLS
  # verification. Installing k3s in that window fails with "curl failed to
  # verify the legitimacy of the server", which looks like a network policy
  # problem and is not one.
  log "waiting for cloud-init to finish..."
  for ip in $(cmd_ips "$prefix"); do
    ssh "${SSH_OPTS[@]}" "root@$ip" "cloud-init status --wait >/dev/null 2>&1 || true" || true
  done
  cmd_ips "$prefix"
}

cmd_ips() {
  api GET "/droplets?tag_name=$1&per_page=200" | pyq "
import json,sys
ds=sorted(json.load(sys.stdin)['droplets'], key=lambda d: d['name'])
for d in ds:
    ip=next((n['ip_address'] for n in d['networks']['v4'] if n['type']=='public'), None)
    if ip: print(ip)
"
}

cmd_privips() {
  api GET "/droplets?tag_name=$1&per_page=200" | pyq "
import json,sys
ds=sorted(json.load(sys.stdin)['droplets'], key=lambda d: d['name'])
for d in ds:
    ip=next((n['ip_address'] for n in d['networks']['v4'] if n['type']=='private'), None)
    if ip: print(ip)
"
}

cmd_ssh() {
  local prefix=$1 n=${2:-1}; shift 2 || true
  local ip; ip=$(cmd_ips "$prefix" | sed -n "${n}p")
  [ -n "$ip" ] || { log "no droplet $prefix-$n"; exit 1; }
  if [ $# -gt 0 ]; then ssh "${SSH_OPTS[@]}" "root@$ip" "$@"; else ssh "${SSH_OPTS[@]}" "root@$ip"; fi
}

# --- airgap ------------------------------------------------------------------
# A DigitalOcean firewall with outbound rules that name only the droplets'
# own subnet. Anything else — get.k3s.io, Docker Hub, the distro mirrors — is
# dropped at DigitalOcean's edge, not by a rule on the host that the install
# script could undo. Inbound SSH from anywhere stays open, or the test could
# not drive the machines at all.
cmd_cut() {
  local prefix=$1
  local ids; ids=$(api GET "/droplets?tag_name=$prefix&per_page=200" | pyq "
import json,sys
print(','.join(str(d['id']) for d in json.load(sys.stdin)['droplets']))
")
  local cidr; cidr=$(cmd_privips "$prefix" | head -1 | sed 's/\.[0-9]*$/.0\/20/')
  log "blocking egress for $prefix (allowing only $cidr and ssh in)..."
  api POST "/firewalls" -d "{
    \"name\": \"$prefix-airgap\",
    \"droplet_ids\": [$ids],
    \"inbound_rules\": [
      {\"protocol\":\"tcp\",\"ports\":\"22\",\"sources\":{\"addresses\":[\"0.0.0.0/0\"]}},
      {\"protocol\":\"tcp\",\"ports\":\"all\",\"sources\":{\"addresses\":[\"$cidr\"]}},
      {\"protocol\":\"udp\",\"ports\":\"all\",\"sources\":{\"addresses\":[\"$cidr\"]}}
    ],
    \"outbound_rules\": [
      {\"protocol\":\"tcp\",\"ports\":\"all\",\"destinations\":{\"addresses\":[\"$cidr\"]}},
      {\"protocol\":\"udp\",\"ports\":\"all\",\"destinations\":{\"addresses\":[\"$cidr\"]}}
    ]
  }" > "$STATE/$prefix.firewall.json"
  pyq "
import json
d=json.load(open('$STATE/$prefix.firewall.json'))
fw=d.get('firewall')
print('firewall', fw['id'], fw['status']) if fw else print('FAILED:', d)
"
}

cmd_mend() {
  local prefix=$1
  local id; id=$(api GET "/firewalls?per_page=200" | pyq "
import json,sys
fs=json.load(sys.stdin)['firewalls']
print(next((f['id'] for f in fs if f['name']=='$prefix-airgap'), ''))
")
  [ -n "$id" ] || { log "no $prefix-airgap firewall"; return 0; }
  api DELETE "/firewalls/$id" >/dev/null
  log "removed $prefix-airgap"
}

# --- teardown ----------------------------------------------------------------
cmd_down() {
  local prefix=$1
  cmd_mend "$prefix" || true
  # Only ever by tag, and only tags this script applies: a typo'd prefix
  # destroys nothing rather than something else.
  local n; n=$(api GET "/droplets?tag_name=$prefix&per_page=200" | pyq "
import json,sys; print(len(json.load(sys.stdin)['droplets']))")
  if [ "$n" = "0" ]; then log "no droplets tagged $prefix"; return 0; fi
  log "destroying $n droplet(s) tagged $prefix..."
  api DELETE "/droplets?tag_name=$prefix" >/dev/null
  rm -f "$STATE/$prefix".*.json "$STATE/$prefix.json"
  log "done"
}

cmd_list() {
  api GET "/droplets?tag_name=$TAG&per_page=200" | pyq "
import json,sys
ds=json.load(sys.stdin)['droplets']
if not ds: print('no k3helper-test droplets'); raise SystemExit
for d in sorted(ds, key=lambda d: d['name']):
    ip=next((n['ip_address'] for n in d['networks']['v4'] if n['type']=='public'), '-')
    print(f\"  {d['name']:24} {d['status']:8} {d['size_slug']:14} {ip}\")
print(f'{len(ds)} droplet(s) running')
"
}

case "${1:-}" in
  up)      shift; cmd_up "$@" ;;
  ips)     shift; cmd_ips "$@" ;;
  privips) shift; cmd_privips "$@" ;;
  ssh)     shift; cmd_ssh "$@" ;;
  cut)     shift; cmd_cut "$@" ;;
  mend)    shift; cmd_mend "$@" ;;
  down)    shift; cmd_down "$@" ;;
  list)    cmd_list ;;
  key)     ensure_key >/dev/null; echo "$KEY" ;;
  *) sed -n '2,20p' "$0"; exit 1 ;;
esac
