#!/bin/bash
# Wait for a commit status to leave pending, for the abuse test (abuse.yml).
# Prints the newest status for CONTEXT as {"state", "description"} JSON;
# state "none" when the forge has none after SECONDS.
#
#   wait-status.sh OWNER/REPO SHA CONTEXT SECONDS
set -u
repo=$1 sha=$2 ctx=$3 limit=$4
end=$((SECONDS + limit))
st='{"state":"none","description":""}'
while [ "$SECONDS" -lt "$end" ]; do
	if all=$(timeout 30 gh api "repos/$repo/commits/$sha/statuses?per_page=100" 2>/dev/null); then
		st=$(jq -c --arg c "$ctx" '[.[] | select(.context == $c)] | first // {state: "none", description: ""} | {state, description}' <<<"$all")
		case $(jq -r .state <<<"$st") in success | failure | error) break ;; esac
	fi
	echo "$(date -u +%T) $ctx: $(jq -r .state <<<"$st")" >&2
	sleep 10
done
printf '%s\n' "$st"
