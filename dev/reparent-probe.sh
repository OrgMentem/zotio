#!/usr/bin/env bash
#
# reparent-probe.sh — does Zotero accept re-parenting an ALREADY-STORED
# attachment, on a library whose files live on personal WebDAV?
#
# Why this exists
# ---------------
# `attachments add` refuses a stored upload on a WebDAV library, because a Web
# API upload always lands in Zotero's own cloud storage, and the desktop
# connector cannot attach to an item that already exists. That leaves no route
# for the commonest repair: an item that exists but is missing its PDF.
#
# The proposed route is: create a temporary parent AND its file in one connector
# session (bytes reach your own file store), re-parent the attachment onto the
# real item, then trash the temporary parent.
#
# Zotero's server source says step 2 is permitted, and says every storage name
# derives from the ATTACHMENT's key rather than its parent — so the bytes should
# not move. See dev/field-report-2026-08-22-papio-round2.md for the citations.
# This script tests that empirically, and settles the one thing the source did
# NOT answer: whether the Zotero DESKTOP client cascades a trash to children.
#
# Safety
# ------
# Every object this script touches is created by this script. It never reads or
# writes an item that existed beforehand. Worst case is a few junk objects in
# your trash, which `zotio items restore` can bring back.
#
# It stops before every write and tells you what to look for. Answering anything
# other than "y" aborts and prints exactly what to clean up.
#
# Requirements
#   ZOTERO_API_KEY   your Zotero Web API key (needed for the re-parent request)
#   PROBE_PDF        optional: a real PDF to use instead of the generated stub
#   Zotero desktop running, with the local API enabled
#   jq, curl
#
# Usage
#   ZOTERO_API_KEY=... ./dev/reparent-probe.sh

set -euo pipefail

STAMP="$(date +%Y%m%d-%H%M%S)"
TAG="ZOTIO REPARENT PROBE ${STAMP}"
WORK="$(mktemp -d)"
STATE="${WORK}/state.env"
: >"${STATE}"

bold() { printf '\n\033[1m%s\033[0m\n' "$*"; }
info() { printf '  %s\n' "$*"; }
warn() { printf '\033[33m  ! %s\033[0m\n' "$*"; }
die() {
	printf '\033[31m\nABORT: %s\033[0m\n' "$*"
	cleanup_hint
	exit 1
}

record() { printf '%s\n' "$1" >>"${STATE}"; }
load() { # shellcheck disable=SC1090
	set -a && . "${STATE}" && set +a
}

cleanup_hint() {
	load 2>/dev/null || true
	bold "State recorded so far"
	if [ -s "${STATE}" ]; then cat "${STATE}"; else info "(nothing created)"; fi
	if [ -n "${PARENT_A:-}" ] || [ -n "${RECEIVER_B:-}" ]; then
		bold "To clean up, trash what this probe created"
		[ -n "${PARENT_A:-}" ] && info "zotio items delete ${PARENT_A} --yes   # temporary parent"
		[ -n "${RECEIVER_B:-}" ] && info "zotio items delete ${RECEIVER_B} --yes   # receiving item"
		info "Both are reversible with 'zotio items restore <key>'."
		info "The attachment follows whichever parent still holds it."
	else
		bold "Nothing to clean up"
		info "No item was created, so your library is untouched."
	fi
	[ -n "${SNAPSHOT:-}" ] && info "Snapshot for reference: ${SNAPSHOT}"
	info "Probe working directory: ${WORK}"
}

checkpoint() {
	bold "CHECK — $1"
	shift
	for line in "$@"; do info "$line"; done
	printf '\n  Continue? [y/N] '
	read -r reply </dev/tty || reply=""
	case "${reply}" in
	y | Y) ;;
	*) die "stopped at your request" ;;
	esac
}

# ---------------------------------------------------------------- preflight ---

bold "Step 0 — preflight"
for bin in zotio jq curl; do
	command -v "${bin}" >/dev/null || die "${bin} is not on PATH"
done
[ -n "${ZOTERO_API_KEY:-}" ] || die "ZOTERO_API_KEY is not set; the re-parent request needs it"

# Resolve the user ID from the key itself. This is the write plane, and an
# independent source from anything zotio has cached.
USER_ID="$(curl -fsS -H "Zotero-API-Key: ${ZOTERO_API_KEY}" \
	https://api.zotero.org/keys/current | jq -r '.userID')" ||
	die "could not resolve your user ID from api.zotero.org"
[ -n "${USER_ID}" ] && [ "${USER_ID}" != "null" ] || die "keys/current returned no userID"
record "USER_ID=${USER_ID}"
info "user ID ${USER_ID}"

API="https://api.zotero.org/users/${USER_ID}"

bold "Your setup, as zotio sees it"
zotio doctor --json 2>/dev/null | jq '{writes, file_storage}' ||
	warn "doctor did not return JSON; check 'zotio doctor' by hand"

checkpoint "your library must keep files on WebDAV for this probe to mean anything" \
	"file_storage above should name webdav. If it says zotero, this probe tests" \
	"the wrong storage backend and its result will not apply to your case." \
	"writes should be available, otherwise the connector step cannot run."

# ----------------------------------------------------------------- snapshot ---

bold "Step 1 — recovery snapshot"
SNAP_OUT="${WORK}/snapshot.jsonl"
if zotio export snapshot --output "${SNAP_OUT}" >"${WORK}/snapshot.log" 2>&1; then
	SNAPSHOT="${SNAP_OUT}"
	record "SNAPSHOT=${SNAPSHOT}"
	info "snapshot written to ${SNAPSHOT} ($(wc -c <"${SNAP_OUT}") bytes)"
else
	warn "snapshot failed: $(tail -1 "${WORK}/snapshot.log")"
	checkpoint "continuing without a snapshot" \
		"Nothing this probe does touches existing data, so a snapshot is a" \
		"belt-and-braces record rather than a requirement."
fi

# ------------------------------------------------- the receiving item (B) -----

bold "Step 2 — create the receiving item"
info "This stands in for 'an item that already exists and is missing its PDF'."

RECV_JSON="$(printf '[{"itemType":"journalArticle","title":"%s — RECEIVER"}]' "${TAG}")"

checkpoint "about to create ONE new junk item" \
	"Title: ${TAG} — RECEIVER" \
	"Nothing existing is touched."

RECV_OUT="${WORK}/receiver.json"
zotio --agent items create --items "${RECV_JSON}" --yes >"${RECV_OUT}" 2>&1 ||
	die "items create failed; see ${RECV_OUT}"

# Three shapes are possible, so try all of them rather than assuming one:
#   zotio connector create -> {"count":1,"key":"KEY","keys":["KEY"],"via":"connector"}
#   Zotero POST /items      -> {"success":{"0":"KEY"},"successful":{"0":{...}}}
#   either, wrapped by zotio's {meta,results} read envelope
# "failed" is never read: a rejected entry can carry a key, and proceeding with
# the key of a create that FAILED would probe a nonexistent item. Written to run
# on both jq and jaq, so it avoids delpaths/walk.
RECEIVER_B="$(jq -r '
    [ .. | objects | (.key? // empty) | select(type=="string") ] as $direct
  | [ .. | objects | (.success? // empty) | .["0"]? // empty ] as $s
  | [ .. | objects | (.successful? // empty) | (.["0"]? // empty) | (.key? // empty) ] as $k
  | ($s + $k + $direct | map(select(type=="string" and length>0))) | first // empty
  ' "${RECV_OUT}" || true)"

if [ -z "${RECEIVER_B}" ] || [ "${RECEIVER_B}" = "null" ]; then
	warn "could not read the new item key automatically. Raw output:"
	cat "${RECV_OUT}"
	printf '\n  Paste the created item key: '
	read -r RECEIVER_B </dev/tty
fi
[ -n "${RECEIVER_B}" ] || die "no receiving item key"
record "RECEIVER_B=${RECEIVER_B}"
info "receiving item ${RECEIVER_B}"

# ----------------------------------- connector create of parent + file (A) ----

bold "Step 3 — create a temporary parent AND its file in one connector session"
info "This is the only route whose bytes reach your own file store."

PROBE_FILE="${PROBE_PDF:-}"
if [ -z "${PROBE_FILE}" ]; then
	PROBE_FILE="${WORK}/probe.pdf"
	# A minimal one-page PDF. This path uses /connector/saveAttachment against a
	# same-session parent, which does NOT invoke Zotero's recognizer, so the
	# file's contents are never parsed for metadata.
	{
		printf '%%PDF-1.4\n'
		printf '1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n'
		printf '2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n'
		printf '3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 99 99]>>endobj\n'
		printf 'trailer<</Root 1 0 R/Size 4>>\n%%%%EOF\n'
	} >"${PROBE_FILE}"
	info "generated stub PDF at ${PROBE_FILE}"
	info "(set PROBE_PDF=/path/to/real.pdf if Zotero rejects the stub)"
else
	[ -f "${PROBE_FILE}" ] || die "PROBE_PDF is not a file: ${PROBE_FILE}"
	info "using your PDF: ${PROBE_FILE}"
fi

MANIFEST="${WORK}/manifest.json"
jq -n --arg p "$(cd "$(dirname "${PROBE_FILE}")" && pwd)/$(basename "${PROBE_FILE}")" \
	--arg t "${TAG} — TEMP PARENT" '
  {schema_version:2, entries:[
    {path:$p, classification:"new", action:"create", status:"resolved",
     item:{itemType:"journalArticle", title:$t}}]}' >"${MANIFEST}"

bold "Preview (no changes yet)"
zotio --agent import apply "${MANIFEST}" --attach-mode stored --via connector ||
	warn "preview returned non-zero; read the message above before continuing"

checkpoint "about to create ONE junk parent plus ONE stored attachment" \
	"Title: ${TAG} — TEMP PARENT" \
	"The bytes go through the running Zotero desktop into your own file store." \
	"Zotero will show its own progress window that zotio cannot dismiss." \
	"This runs ONCE. Do not loop this step."

APPLY_OUT="${WORK}/apply.json"
zotio --agent import apply "${MANIFEST}" --attach-mode stored --via connector --yes \
	>"${APPLY_OUT}" 2>&1 || {
	cat "${APPLY_OUT}"
	die "import apply failed; if a parent was created without its file, the message above names it"
}
info "applied; output in ${APPLY_OUT}"

# The connector branch reports only {"via":"connector"} — no item key and no
# attachment key. Both have to be discovered, which is itself a finding: any
# real implementation of this route needs a discovery step.
bold "Step 4 — discover the keys the connector did not return"
info "Waiting 20s: propagation between the desktop read plane and api.zotero.org"
sleep 20

PARENT_A="$(curl -fsS -H "Zotero-API-Key: ${ZOTERO_API_KEY}" \
	--get --data-urlencode "q=${TAG} — TEMP PARENT" --data-urlencode "qmode=titleCreatorYear" \
	--data-urlencode "limit=5" "${API}/items" |
	jq -r --arg t "${TAG} — TEMP PARENT" \
		'[.[] | select(.data.title==$t) | .key] | first // empty')" || true

if [ -z "${PARENT_A}" ]; then
	warn "could not find the temporary parent by title."
	printf '  Paste the temporary parent key (from Zotero): '
	read -r PARENT_A </dev/tty
fi
[ -n "${PARENT_A}" ] || die "no temporary parent key"
record "PARENT_A=${PARENT_A}"
info "temporary parent ${PARENT_A}"

ATTACH="$(curl -fsS -H "Zotero-API-Key: ${ZOTERO_API_KEY}" \
	"${API}/items/${PARENT_A}/children" |
	jq -r '[.[] | select(.data.itemType=="attachment") | .key] | first // empty')" || true
if [ -z "${ATTACH}" ]; then
	warn "the temporary parent has no attachment child. The file did not land."
	die "nothing to re-parent"
fi
record "ATTACH=${ATTACH}"

ATTACH_MODE="$(curl -fsS -H "Zotero-API-Key: ${ZOTERO_API_KEY}" \
	"${API}/items/${ATTACH}" | jq -r '.data.linkMode')"
info "attachment ${ATTACH} (linkMode ${ATTACH_MODE})"
[ "${ATTACH_MODE}" = "imported_file" ] || [ "${ATTACH_MODE}" = "imported_url" ] ||
	warn "linkMode is ${ATTACH_MODE}; this probe is about STORED files"

# ------------------------------------------------ WebDAV oracle, before -------

checkpoint "sync Zotero, then check your WebDAV server DIRECTLY" \
	"Press sync in Zotero and let it finish." \
	"" \
	"On your WebDAV server, inside the zotero/ directory, you should now see:" \
	"    ${ATTACH}.zip" \
	"    ${ATTACH}.prop" \
	"" \
	"Both names come from the ATTACHMENT's key, never its parent — that is the" \
	"property the re-parent depends on. Confirm they exist before continuing," \
	"otherwise the next step tests a file that never reached WebDAV, which is" \
	"the weaker claim and not your case." \
	"" \
	"Check the server itself, not another zotio command: an independent oracle."

# ------------------------------------------------------------- the re-parent ---

bold "Step 5 — re-parent the attachment"
VERSION="$(curl -fsS -H "Zotero-API-Key: ${ZOTERO_API_KEY}" \
	"${API}/items/${ATTACH}" | jq -r '.version')"
[ -n "${VERSION}" ] && [ "${VERSION}" != "null" ] || die "could not read the attachment's version"
info "attachment version ${VERSION}"

checkpoint "about to move attachment ${ATTACH} from ${PARENT_A} to ${RECEIVER_B}" \
	"PATCH ${API}/items/${ATTACH}" \
	'Body: {"parentItem":"'"${RECEIVER_B}"'"}' \
	"Guarded by If-Unmodified-Since-Version: ${VERSION}." \
	"A 412 here means something else changed the item; that is the guard working."

HTTP="$(curl -sS -o "${WORK}/patch.out" -w '%{http_code}' \
	-X PATCH "${API}/items/${ATTACH}" \
	-H "Zotero-API-Key: ${ZOTERO_API_KEY}" \
	-H "Content-Type: application/json" \
	-H "If-Unmodified-Since-Version: ${VERSION}" \
	-d "{\"parentItem\":\"${RECEIVER_B}\"}")"

bold "Re-parent result: HTTP ${HTTP}"
cat "${WORK}/patch.out" 2>/dev/null || true
echo
case "${HTTP}" in
20*)
	info "Zotero ACCEPTED the re-parent."
	;;
412)
	die "precondition failed: the attachment changed under us. Re-run the probe."
	;;
*)
	warn "Zotero REFUSED the re-parent with HTTP ${HTTP}."
	warn "That is the answer: the route does not work, and the refusal in"
	warn "attachments add should be restated as a permanent property."
	cleanup_hint
	exit 2
	;;
esac

# --------------------------------------------------------------- verify -------

bold "Step 6 — verify the move server-side"
sleep 5
NEW_PARENT="$(curl -fsS -H "Zotero-API-Key: ${ZOTERO_API_KEY}" \
	"${API}/items/${ATTACH}" | jq -r '.data.parentItem // "none"')"
info "attachment's parentItem is now: ${NEW_PARENT}"
[ "${NEW_PARENT}" = "${RECEIVER_B}" ] ||
	warn "expected ${RECEIVER_B}; the write reported success but did not stick"

A_KIDS="$(curl -fsS -H "Zotero-API-Key: ${ZOTERO_API_KEY}" \
	"${API}/items/${PARENT_A}/children" | jq 'length')"
info "temporary parent now has ${A_KIDS} child(ren) — expected 0"

checkpoint "sync Zotero, then check the FILE and WebDAV again" \
	"Press sync in Zotero and let it finish." \
	"" \
	"1. In Zotero, the attachment now sits under:" \
	"     ${TAG} — RECEIVER" \
	"2. Double-click it. THE FILE MUST STILL OPEN. This is the whole point:" \
	"   re-parenting must not strand the bytes." \
	"3. On your WebDAV server, you should still see EXACTLY ONE pair," \
	"   with the SAME names as before:" \
	"     ${ATTACH}.zip" \
	"     ${ATTACH}.prop" \
	"   No orphan, no duplicate, no rename."

# ------------------------------- the unknown: does trash cascade locally? -----

bold "Step 7 — the question the source could not answer"
info "Zotero's SERVER shows no child cascade when a parent is trashed."
info "The DESKTOP CLIENT is separate code that was not inspected."
info "The attachment has already been moved, so this should be safe."

checkpoint "about to trash ONLY the temporary parent ${PARENT_A}" \
	"The attachment now belongs to ${RECEIVER_B}, not to this item." \
	"'zotio items restore ${PARENT_A}' reverses it."

zotio --agent items delete "${PARENT_A}" --yes >"${WORK}/trash.json" 2>&1 ||
	warn "trashing the temporary parent failed; see ${WORK}/trash.json"

sleep 5
STILL="$(curl -fsS -H "Zotero-API-Key: ${ZOTERO_API_KEY}" \
	"${API}/items/${ATTACH}" | jq -r '.data.parentItem // "none"')"
DELETED="$(curl -fsS -H "Zotero-API-Key: ${ZOTERO_API_KEY}" \
	"${API}/items/${ATTACH}" | jq -r '.data.deleted // 0')"

bold "Result after trashing the old parent"
info "attachment parentItem: ${STILL}   (want ${RECEIVER_B})"
info "attachment deleted flag: ${DELETED}   (want 0)"
if [ "${STILL}" = "${RECEIVER_B}" ] && [ "${DELETED}" = "0" ]; then
	info "No cascade. Trashing the old parent left the moved attachment alone."
else
	warn "The attachment was affected by trashing its former parent."
	warn "Re-parent BEFORE trashing is then not sufficient on its own."
fi

checkpoint "sync Zotero, then confirm in the desktop client" \
	"Press sync and let it finish." \
	"The attachment must still be present and openable under:" \
	"    ${TAG} — RECEIVER" \
	"The server says one thing; the client is what you actually use."

# -------------------------------------------------------------- cleanup -------

bold "Step 8 — clean up"
checkpoint "about to trash the receiving item ${RECEIVER_B} and its attachment" \
	"This removes the last object the probe created." \
	"Reversible with 'zotio items restore ${RECEIVER_B}'."

zotio --agent items delete "${RECEIVER_B}" --yes >"${WORK}/trash-b.json" 2>&1 ||
	warn "trashing the receiving item failed; see ${WORK}/trash-b.json"

bold "Done"
info "Everything the probe created is now in your trash."
info "Empty the trash in Zotero when you are satisfied, and sync once more so"
info "the WebDAV files are removed too."
info ""
info "Keys, for the record:"
cat "${STATE}"
info ""
info "Working directory kept for inspection: ${WORK}"
