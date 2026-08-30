#!/usr/bin/env bash
#
# reparent-probe.sh — does `zotio attachments add --via connector` actually
# attach a stored file to an item that ALREADY EXISTS, on a library whose files
# live on personal WebDAV?
#
# Why this exists
# ---------------
# A Web API stored upload always lands in Zotero's own cloud storage, and the
# desktop connector cannot attach to an item that already exists
# (POST /connector/saveAttachment resolves parentItemID only through its own
# save session). That left no route for the commonest repair: an item that
# exists but is missing its PDF.
#
# `attachments add <key> <file> --via connector` is that route. In ONE connector
# session it creates a temporary parent plus the file (so the bytes reach your
# own file store), moves the attachment with a single
# PATCH {"parentItem": …} guarded by If-Unmodified-Since-Version, then trashes
# the temporary parent. Re-parenting relocates no bytes, because every storage
# name — the local directory, the upload zip, and both WebDAV remote names —
# derives from the ATTACHMENT's key rather than its parent's.
#
# History of this script. It first proved the ROUTE, before the feature existed,
# by driving the choreography by hand and issuing the re-parent PATCH with curl.
# That answered "does Zotero permit this" — see
# dev/field-report-2026-08-22-papio-round2.md for the citations and the live
# HTTP 204. The route then shipped as a command, which made the hand
# choreography the wrong thing to test: it exercised Zotero rather than zotio.
# So the probe now drives the SHIPPED COMMAND end to end and checks the result
# against an independent oracle.
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
#   ZOTERO_API_KEY   your Zotero Web API key (the re-parent PATCH needs it)
#   PROBE_PDF        optional: a real PDF to use instead of the generated stub
#   Zotero desktop running, with the local API enabled
#   jq, curl
#
# Usage
#   ZOTERO_API_KEY=... ./dev/reparent-probe.sh
#
#   PROBE_YES=1 auto-confirms every checkpoint, for an automated rehearsal. The
#   checkpoints that ask YOU to look at Zotero or at your WebDAV server cannot be
#   automated; under PROBE_YES they are printed as SKIPPED, and what they would
#   have verified is left unestablished rather than silently assumed.

set -euo pipefail

STAMP="$(date +%Y%m%d-%H%M%S)"
TAG="ZOTIO REPARENT PROBE ${STAMP}"
WORK="$(mktemp -d)"
STATE="${WORK}/state.env"
: >"${STATE}"

bold() { printf '\n\033[1m%s\033[0m\n' "$*"; }
info() { printf '  %s\n' "$*"; }
warn() { printf '\033[33m  ! %s\033[0m\n' "$*"; }
skip() { printf '\033[36m  SKIPPED (PROBE_YES): %s\033[0m\n' "$*"; }
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
	if [ -n "${RECEIVER_B:-}" ] || [ -n "${TEMP_PARENT:-}" ]; then
		bold "To clean up, trash what this probe created"
		[ -n "${RECEIVER_B:-}" ] && info "zotio items delete ${RECEIVER_B} --yes   # receiving item"
		[ -n "${TEMP_PARENT:-}" ] && info "zotio items delete ${TEMP_PARENT} --yes   # temporary parent, if the command left it"
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
	if [ "${PROBE_YES:-}" = "1" ]; then
		info "auto-confirmed (PROBE_YES=1)"
		return
	fi
	printf '\n  Continue? [y/N] '
	read -r reply </dev/tty || reply=""
	case "${reply}" in
	y | Y) ;;
	*) die "stopped at your request" ;;
	esac
}

# A checkpoint that only YOU can satisfy: it asks you to look at the Zotero
# desktop or at your WebDAV server. Automating it is impossible, so under
# PROBE_YES it is announced as skipped and its claim stays unestablished.
manual_checkpoint() {
	if [ "${PROBE_YES:-}" = "1" ]; then
		bold "MANUAL CHECK — $1"
		shift
		for line in "$@"; do info "$line"; done
		skip "requires a human looking at Zotero or at the WebDAV server"
		return
	fi
	checkpoint "$@"
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

# stdout and stderr separated here for the same reason as step 3 below: a
# stderr routing notice folded into the JSON breaks every extraction.
RECV_OUT="${WORK}/receiver.json"
RECV_ERR="${WORK}/receiver.err"
zotio --agent items create --items "${RECV_JSON}" --yes >"${RECV_OUT}" 2>"${RECV_ERR}" ||
	die "items create failed; stdout ${RECV_OUT}, stderr ${RECV_ERR}: $(tail -1 "${RECV_ERR}" 2>/dev/null)"

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

# ------------------------------------------------------------- the file -------

PROBE_FILE="${PROBE_PDF:-}"
if [ -z "${PROBE_FILE}" ]; then
	PROBE_FILE="${WORK}/probe.pdf"
	# A minimal one-page PDF. The connector may reject a stub; PROBE_PDF exists
	# so you can hand it a real one.
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
PROBE_ABS="$(cd "$(dirname "${PROBE_FILE}")" && pwd)/$(basename "${PROBE_FILE}")"
PROBE_MD5="$(md5sum <"${PROBE_ABS}" | cut -d' ' -f1)"
PROBE_BYTES="$(wc -c <"${PROBE_ABS}" | tr -d ' ')"
record "PROBE_MD5=${PROBE_MD5}"
info "file md5 ${PROBE_MD5} (${PROBE_BYTES} bytes)"

# ------------------------------------------- the command under test -----------

bold "Step 3 — attach the file to the EXISTING item, in one command"
info "zotio attachments add ${RECEIVER_B} <file> --via connector"
info "Internally: connector session creates a temporary parent plus the file,"
info "then one guarded PATCH moves the attachment, then the temporary parent is"
info "trashed. This is the command under test — not a hand-rolled equivalent."

bold "Preview (no changes yet)"
zotio --agent attachments add "${RECEIVER_B}" "${PROBE_ABS}" --via connector ||
	warn "preview returned non-zero; read the message above before continuing"

checkpoint "about to attach ONE stored file to ${RECEIVER_B}" \
	"The bytes go through the running Zotero desktop into your own file store." \
	"A temporary parent is created and trashed as part of the route." \
	"Zotero will show its own progress window that zotio cannot dismiss." \
	"This runs ONCE. Do not loop this step."

# stdout and stderr MUST go to different files. zotio prints a one-time routing
# notice to stderr ("→ writing via Zotero Web API: …"); folding it into the JSON
# with 2>&1 makes every jq extraction below silently return empty, which reads
# exactly like a command that returned no keys. That happened on the first live
# run: the route had fully succeeded and the probe reported "did not report
# success", then skipped its own cleanup.
ADD_OUT="${WORK}/add.json"
ADD_ERR="${WORK}/add.err"
ADD_RC=0
zotio --agent attachments add "${RECEIVER_B}" "${PROBE_ABS}" --via connector --yes \
	>"${ADD_OUT}" 2>"${ADD_ERR}" || ADD_RC=$?
info "exit ${ADD_RC}; stdout in ${ADD_OUT}, stderr in ${ADD_ERR}"
[ -s "${ADD_ERR}" ] && sed 's/^/  stderr: /' "${ADD_ERR}"
cat "${ADD_OUT}"

# Fail loudly if stdout is not JSON, rather than letting every extraction below
# return empty and be misread as "the command reported nothing".
jq -e . "${ADD_OUT}" >/dev/null 2>&1 || {
	warn "stdout is not valid JSON; the extractions below cannot be trusted"
	cleanup_hint
	exit 4
}

# Pull the route's own report of what it did. Absent fields stay empty rather
# than defaulting, so a missing key is visible instead of being read as "none".
ATTACH="$(jq -r '[.. | objects | (.attachment_key? // empty)] | map(select(type=="string" and length>0)) | first // empty' "${ADD_OUT}" 2>/dev/null || true)"
TEMP_PARENT="$(jq -r '[.. | objects | (.temp_parent_key? // empty)] | map(select(type=="string" and length>0)) | first // empty' "${ADD_OUT}" 2>/dev/null || true)"
TEMP_TRASHED="$(jq -r '[.. | objects | (.temp_parent_trashed? // empty)] | first // empty' "${ADD_OUT}" 2>/dev/null || true)"
STATUS="$(jq -r '[.. | objects | (.status? // empty)] | map(select(type=="string")) | first // empty' "${ADD_OUT}" 2>/dev/null || true)"
[ -n "${ATTACH}" ] && record "ATTACH=${ATTACH}"
[ -n "${TEMP_PARENT}" ] && record "TEMP_PARENT=${TEMP_PARENT}"

bold "What the command reported"
info "status              ${STATUS:-<none>}"
info "attachment_key      ${ATTACH:-<none>}"
info "temp_parent_key     ${TEMP_PARENT:-<none>}"
info "temp_parent_trashed ${TEMP_TRASHED:-<none>}"

if [ "${ADD_RC}" -ne 0 ] || [ "${STATUS}" != "applied" ]; then
	warn "The command did not report success."
	warn "If temp_parent_trashed is false, the route refused to trash a parent"
	warn "whose attachment was still beneath it — that is fail-closed, not a leak."
	cleanup_hint
	exit 2
fi

# --------------------------------------------------- independent oracle -------

bold "Step 4 — verify against api.zotero.org directly"
info "The server, not another zotio command."
sleep 5

[ -n "${ATTACH}" ] || die "the command reported success but returned no attachment key"

A_JSON="${WORK}/attach.json"
curl -fsS -H "Zotero-API-Key: ${ZOTERO_API_KEY}" "${API}/items/${ATTACH}" >"${A_JSON}" ||
	die "could not read attachment ${ATTACH} from the write plane"

NEW_PARENT="$(jq -r '.data.parentItem // "none"' "${A_JSON}")"
A_DELETED="$(jq -r '.data.deleted // 0' "${A_JSON}")"
A_MD5="$(jq -r '.data.md5 // "none"' "${A_JSON}")"
A_MODE="$(jq -r '.data.linkMode // "none"' "${A_JSON}")"

info "parentItem  ${NEW_PARENT}   (want ${RECEIVER_B})"
info "deleted     ${A_DELETED}   (want 0)"
info "md5         ${A_MD5}"
info "            ${PROBE_MD5}   (the file on disk)"
info "linkMode    ${A_MODE}   (want imported_file or imported_url)"

FAILED=0
[ "${NEW_PARENT}" = "${RECEIVER_B}" ] || {
	warn "the attachment did NOT end up on the receiving item"
	FAILED=1
}
[ "${A_DELETED}" = "0" ] || {
	warn "the attachment is trashed; the temporary parent's trash cascaded"
	FAILED=1
}
[ "${A_MD5}" = "${PROBE_MD5}" ] || {
	warn "the registered md5 does not match the file that was uploaded"
	FAILED=1
}

if [ -n "${TEMP_PARENT}" ]; then
	T_DELETED="$(curl -fsS -H "Zotero-API-Key: ${ZOTERO_API_KEY}" \
		"${API}/items/${TEMP_PARENT}" | jq -r '.data.deleted // 0')"
	T_KIDS="$(curl -fsS -H "Zotero-API-Key: ${ZOTERO_API_KEY}" \
		"${API}/items/${TEMP_PARENT}/children" | jq 'length')"
	info "temp parent deleted  ${T_DELETED}   (want 1)"
	info "temp parent children ${T_KIDS}   (want 0)"
	[ "${T_DELETED}" = "1" ] || {
		warn "the temporary parent was left in the library"
		FAILED=1
	}
	[ "${T_KIDS}" = "0" ] || {
		warn "the temporary parent still has children"
		FAILED=1
	}
fi

# The storage-naming invariant, checked locally: the directory must be named for
# the ATTACHMENT's key, never its parent's. This is what makes re-parenting free.
STORAGE_DIR=""
for cand in "${HOME}/Zotero/storage" "${ZOTERO_DATA_DIR:-}/storage"; do
	[ -n "${cand}" ] && [ -d "${cand}" ] && STORAGE_DIR="${cand}" && break
done
if [ -n "${STORAGE_DIR}" ]; then
	if [ -d "${STORAGE_DIR}/${ATTACH}" ]; then
		info "storage dir ${STORAGE_DIR}/${ATTACH} exists — named for the ATTACHMENT"
		find "${STORAGE_DIR}/${ATTACH}" -maxdepth 1 -mindepth 1 -exec basename {} \; | sed 's/^/      /'
	else
		warn "no storage directory named ${ATTACH}; the desktop may not have"
		warn "written it yet, or your data directory is elsewhere"
	fi
	[ -n "${TEMP_PARENT}" ] && [ -d "${STORAGE_DIR}/${TEMP_PARENT}" ] && {
		warn "a storage directory exists for the TEMPORARY PARENT key — the"
		warn "naming invariant this route depends on does not hold"
		FAILED=1
	}
else
	warn "could not locate a Zotero storage directory; skipped the naming check"
fi

[ "${FAILED}" = "0" ] || {
	warn "verification failed; see above"
	cleanup_hint
	exit 3
}
bold "Server-side verification PASSED"

manual_checkpoint "sync Zotero, then check the FILE and WebDAV" \
	"Press sync in Zotero and let it finish." \
	"" \
	"1. In Zotero, the attachment sits under: ${TAG} — RECEIVER" \
	"2. Double-click it. THE FILE MUST STILL OPEN. This is the whole point:" \
	"   re-parenting must not strand the bytes." \
	"3. On your WebDAV server, inside zotero/, you should see EXACTLY ONE pair:" \
	"     ${ATTACH}.zip" \
	"     ${ATTACH}.prop" \
	"   Both names come from the ATTACHMENT's key, never its parent." \
	"   No orphan, no duplicate, no rename." \
	"" \
	"Check the server itself, not another zotio command: an independent oracle."

# -------------------------------------------------------------- cleanup -------

bold "Step 5 — clean up"
checkpoint "about to trash the attachment ${ATTACH} and the receiving item ${RECEIVER_B}" \
	"This removes the last objects the probe created." \
	"Reversible with 'zotio items restore <key>'."

# The attachment MUST be trashed explicitly, and first. Zotero's server does not
# cascade a trash to children: trashing only the parent leaves the attachment
# LIVE, so it keeps appearing in /items while its parent sits in the trash.
# Measured on 2026-08-30 — three probe runs left three live orphan attachments,
# which a revert detector caught as +3 items against a baseline that was
# otherwise byte-identical. Trashing the child first also matches the route's own
# ordering rule: never let an attachment follow a parent into the trash.
zotio --agent items delete "${ATTACH}" --yes >"${WORK}/trash-attach.json" 2>"${WORK}/trash-attach.err" ||
	warn "trashing the attachment failed; see ${WORK}/trash-attach.err"

zotio --agent items delete "${RECEIVER_B}" --yes >"${WORK}/trash-b.json" 2>"${WORK}/trash-b.err" ||
	warn "trashing the receiving item failed; see ${WORK}/trash-b.err"

bold "Done"
info "Everything the probe created is now in your trash."
info "Empty the trash in Zotero when you are satisfied, and sync once more so"
info "the WebDAV files are removed too."
info ""
info "Verify nothing was left live:"
info "  zotio items trash --data-source live --json   # the probe's objects should be here"
info ""
info "Keys, for the record:"
cat "${STATE}"
info ""
info "Working directory kept for inspection: ${WORK}"
