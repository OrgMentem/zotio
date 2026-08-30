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
# writes an item that existed beforehand. Worst case is recoverable junk objects.
# The recovery hint records known keys and prints before every abnormal exit.
#
# It stops before every write and tells you what to look for. Answering anything
# other than "y" aborts and prints exactly what to clean up.
#
# Requirements
#   ZOTERO_API_KEY   your Zotero Web API key (the re-parent PATCH needs it)
#   PROBE_PDF        optional: a real PDF to use instead of the generated stub
#   Zotero desktop running, with the local API enabled
#   jq, curl, md5 or md5sum
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
	CLEANUP_REPORTED=1
	load 2>/dev/null || true
	bold "State recorded so far"
	if [ -s "${STATE}" ]; then cat "${STATE}"; else info "(nothing created)"; fi
	if [ -n "${ATTACH:-}" ] || [ -n "${TEMP_PARENT:-}" ] || [ -n "${RECEIVER_B:-}" ]; then
		bold "To clean up, trash what this probe created"
		[ -n "${ATTACH:-}" ] && info "zotio items delete ${ATTACH} --yes   # attachment; delete first"
		[ -n "${TEMP_PARENT:-}" ] && info "zotio items delete ${TEMP_PARENT} --yes   # temporary parent"
		[ -n "${RECEIVER_B:-}" ] && info "zotio items delete ${RECEIVER_B} --yes   # receiving item"
		info "All are reversible with 'zotio items restore <key>'."
		[ "${TEMP_PARENT_MISSING:-0}" = "1" ] &&
			warn "the command omitted its temporary parent key; do not assume it was trashed"
		[ -z "${ATTACH:-}" ] && [ -n "${RECEIVER_TITLE:-}" ] &&
			warn "if the route was interrupted, inspect ${RECEIVER_TITLE} for its attachment"
	elif [ -n "${RECEIVER_TITLE:-}" ]; then
		bold "The receiving item may need recovery"
		warn "Search Zotero for the exact title: ${RECEIVER_TITLE}"
		warn "The create command did not report a key, so this probe cannot name it."
	else
		bold "Nothing to clean up"
		info "No item was created, so your library is untouched."
	fi
	[ -n "${SNAPSHOT:-}" ] && info "Snapshot for reference: ${SNAPSHOT}"
	info "Probe working directory: ${WORK}"
}

# Print recovery keys for every abnormal exit. The route can create objects
# before it returns, so an interrupt must not discard the only recovery record.
on_exit() {
	local rc=$?
	if [ "${rc}" -ne 0 ] && [ "${CLEANUP_REPORTED:-0}" != "1" ]; then
		cleanup_hint
	fi
	exit "${rc}"
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

receiver_by_title() {
	curl -fsSG -H "Zotero-API-Key: ${ZOTERO_API_KEY}" \
		--data-urlencode "q=${RECEIVER_TITLE}" \
		--data "qmode=title" "${API}/items" |
		jq -r --arg title "${RECEIVER_TITLE}" \
			'[.[] | select(.data.title == $title) | .key] | unique |
			 if length == 1 then .[0] else empty end'
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
if command -v md5 >/dev/null; then
	HASHER=md5
elif command -v md5sum >/dev/null; then
	HASHER=md5sum
else
	die "neither md5 nor md5sum is on PATH"
fi
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
DOCTOR_JSON="${WORK}/doctor.json"
zotio doctor --json >"${DOCTOR_JSON}" 2>"${WORK}/doctor.err" ||
	die "doctor did not return JSON; see ${WORK}/doctor.err"
jq '{writes, file_storage}' "${DOCTOR_JSON}" ||
	die "doctor returned invalid JSON; see ${DOCTOR_JSON}"
FILE_STORAGE="$(jq -er '.file_storage | strings' "${DOCTOR_JSON}")" ||
	die "doctor returned no usable file_storage value"
# doctor's file_storage is a human-readable sentence, not an enum: it leads with
# the mode and then names the host and the consequence, e.g.
#   "WebDAV (host) (personal library) — stored uploads ... are refused".
# An equality test against "webdav" can therefore never hold. Match the leading
# mode word so the check still fails closed on a Zotero-cloud store, which is
# the case this probe must refuse: it would bill the operator's plan.
case "${FILE_STORAGE}" in
[Ww][Ee][Bb][Dd][Aa][Vv]*) ;;
*) die "doctor reports file_storage=${FILE_STORAGE}; this probe requires a WebDAV file store" ;;
esac

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

RECEIVER_TITLE="${TAG} — RECEIVER"
RECV_JSON="$(printf '[{"itemType":"journalArticle","title":"%s"}]' "${RECEIVER_TITLE}")"

checkpoint "about to create ONE new junk item" \
	"Title: ${RECEIVER_TITLE}" \
	"Nothing existing is touched."

# stdout and stderr separated here for the same reason as step 3 below: a
# stderr routing notice folded into the JSON breaks every extraction.
RECV_OUT="${WORK}/receiver.json"
RECV_ERR="${WORK}/receiver.err"
RECV_RC=0
zotio --agent items create --items "${RECV_JSON}" --yes >"${RECV_OUT}" 2>"${RECV_ERR}" ||
	RECV_RC=$?

# A connector create can commit before its confirmation lookup reports an error.
# Search the write plane by the exact title so the recovery hint still has a key.
if [ "${RECV_RC}" -ne 0 ]; then
	if ! RECEIVER_B="$(receiver_by_title)"; then
		die "items create failed and the write plane could not reconcile ${RECEIVER_TITLE}"
	fi
	if [ -n "${RECEIVER_B}" ]; then
		record "RECEIVER_B=${RECEIVER_B}"
	fi
	die "items create failed; stdout ${RECV_OUT}, stderr ${RECV_ERR}: $(tail -1 "${RECV_ERR}" 2>/dev/null)"
fi

# Three shapes are possible, so try all of them rather than assuming one:
#   zotio connector create -> {"count":1,"key":"KEY","keys":["KEY"],"via":"connector"}
#   Zotero POST /items      -> {"success":{"0":"KEY"},"successful":{"0":{...}}}
#   either, wrapped by zotio's {meta,results} read envelope
# Only success envelopes are read. A failed entry can carry a key, but that key
# does not name an item this probe may attach to or later trash.
if ! RECEIVER_B="$(jq -r '
	def result_keys:
		[
			.key?,
			.keys?[0]?,
			.success?["0"]?,
			.successful?["0"]?.key?,
			(.results? | if type == "object" then result_keys[] else empty end)
		];
	[result_keys[] | select(type == "string" and length > 0)] | first // empty
	' "${RECV_OUT}")"; then
	die "could not extract the receiving item key from ${RECV_OUT}"
fi

if [ -z "${RECEIVER_B}" ] || [ "${RECEIVER_B}" = "null" ]; then
	warn "could not read the new item key automatically. Raw output:"
	cat "${RECV_OUT}"
	if [ "${PROBE_YES:-}" = "1" ]; then
		die "items create returned no key under PROBE_YES; no TTY fallback is safe"
	fi
	printf '\n  Paste the created item key: '
	read -r RECEIVER_B </dev/tty ||
		die "could not read a receiving item key from the TTY"
fi
[ -n "${RECEIVER_B}" ] || die "no receiving item key"
[[ "${RECEIVER_B}" =~ ^[A-Z0-9]{8}$ ]] ||
	die "receiving item key is not a Zotero key: ${RECEIVER_B}"
record "RECEIVER_B=${RECEIVER_B}"

# Confirm the key identifies the receiver this run created. This protects every
# existing library item from a mistaken paste during the final cleanup.
RECEIVER_JSON="${WORK}/receiver-verify.json"
# The GET is retried. api.zotero.org hands back the new key before that item is
# readable on this endpoint, so a single-shot check reports 404 for a receiver
# that was in fact created correctly a moment earlier — measured on 2026-08-31.
# The identity assertion below is unchanged; only its input is now awaited.
RECEIVER_VERIFIED=0
for _ in 1 2 3 4 5 6; do
	if curl -fsS -H "Zotero-API-Key: ${ZOTERO_API_KEY}" \
		"${API}/items/${RECEIVER_B}" >"${RECEIVER_JSON}" 2>/dev/null; then
		RECEIVER_VERIFIED=1
		break
	fi
	sleep 3
done
[ "${RECEIVER_VERIFIED}" = "1" ] ||
	die "could not verify receiving item ${RECEIVER_B} on the write plane"
RECEIVER_ACTUAL_TITLE="$(jq -er '.data.title | strings' "${RECEIVER_JSON}")" ||
	die "could not read the title of receiving item ${RECEIVER_B}"
[ "${RECEIVER_ACTUAL_TITLE}" = "${RECEIVER_TITLE}" ] ||
	die "receiving item ${RECEIVER_B} is not this probe's receiver"
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
if ! PROBE_DIR="$(cd "$(dirname "${PROBE_FILE}")" && pwd)"; then
	die "could not resolve the directory for ${PROBE_FILE}"
fi
PROBE_ABS="${PROBE_DIR}/$(basename "${PROBE_FILE}")"
if [ "${HASHER}" = "md5" ]; then
	PROBE_MD5="$(md5 -q "${PROBE_ABS}")" ||
		die "could not hash ${PROBE_ABS} with md5"
else
	PROBE_MD5="$(md5sum "${PROBE_ABS}" | cut -d' ' -f1)" ||
		die "could not hash ${PROBE_ABS} with md5sum"
fi
PROBE_BYTES="$(wc -c <"${PROBE_ABS}" | tr -d ' ')" ||
	die "could not count bytes in ${PROBE_ABS}"
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
if ! jq -e . "${ADD_OUT}" >/dev/null 2>&1; then
	die "stdout is not valid JSON; the extractions cannot be trusted"
fi

# Pull the route's own report of what it did. Absent fields stay empty rather
# than defaulting, so a missing key is visible instead of being read as "none".
if ! ATTACH="$(jq -r '[.. | objects | (.attachment_key? // empty)] |
	map(select(type == "string" and length > 0)) | first // empty' "${ADD_OUT}")"; then
	die "could not extract attachment_key from ${ADD_OUT}"
fi
if ! TEMP_PARENT="$(jq -r '[.. | objects | (.temp_parent_key? // empty)] |
	map(select(type == "string" and length > 0)) | first // empty' "${ADD_OUT}")"; then
	die "could not extract temp_parent_key from ${ADD_OUT}"
fi
if ! TEMP_TRASHED="$(jq -r '[.. | objects | (.temp_parent_trashed? // empty)] |
	first // empty' "${ADD_OUT}")"; then
	die "could not extract temp_parent_trashed from ${ADD_OUT}"
fi
if ! STATUS="$(jq -r '[.. | objects | (.status? // empty)] |
	map(select(type == "string")) | first // empty' "${ADD_OUT}")"; then
	die "could not extract status from ${ADD_OUT}"
fi
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
if [ -z "${TEMP_PARENT}" ]; then
	TEMP_PARENT_MISSING=1
	die "applied result omitted temp_parent_key; the temporary parent cannot be checked or cleaned"
fi

# --------------------------------------------------- independent oracle -------

bold "Step 4 — verify against api.zotero.org directly"
info "The server, not another zotio command."
sleep 5

[ -n "${ATTACH}" ] || die "the command reported success but returned no attachment key"

A_JSON="${WORK}/attach.json"
curl -fsS -H "Zotero-API-Key: ${ZOTERO_API_KEY}" "${API}/items/${ATTACH}" >"${A_JSON}" ||
	die "could not read attachment ${ATTACH} from the write plane"

if ! NEW_PARENT="$(jq -r '.data.parentItem // "none"' "${A_JSON}")"; then
	die "could not read parentItem from ${A_JSON}"
fi
if ! A_DELETED="$(jq -r '.data.deleted // 0' "${A_JSON}")"; then
	die "could not read deleted from ${A_JSON}"
fi
if ! A_MD5="$(jq -r '.data.md5 // "none"' "${A_JSON}")"; then
	die "could not read md5 from ${A_JSON}"
fi
if ! A_MODE="$(jq -r '.data.linkMode // "none"' "${A_JSON}")"; then
	die "could not read linkMode from ${A_JSON}"
fi

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
[ "${A_MODE}" = "imported_file" ] || [ "${A_MODE}" = "imported_url" ] || {
	warn "the attachment has an unexpected linkMode"
	FAILED=1
}

if ! T_DELETED="$(curl -fsS -H "Zotero-API-Key: ${ZOTERO_API_KEY}" \
	"${API}/items/${TEMP_PARENT}" | jq -r '.data.deleted // 0')"; then
	die "could not read the temporary parent ${TEMP_PARENT} from the write plane"
fi
if ! T_KIDS="$(curl -fsS -H "Zotero-API-Key: ${ZOTERO_API_KEY}" \
	"${API}/items/${TEMP_PARENT}/children" | jq 'length')"; then
	die "could not read children of temporary parent ${TEMP_PARENT}"
fi
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

# The storage-naming invariant, checked locally: the directory must be named for
# the ATTACHMENT's key, never its parent's. This is what makes re-parenting free.
STORAGE_DIR=""
if [ -n "${ZOTERO_DATA_DIR:-}" ]; then
	STORAGE_CANDIDATE="${ZOTERO_DATA_DIR}/storage"
elif [ -n "${HOME:-}" ]; then
	STORAGE_CANDIDATE="${HOME}/Zotero/storage"
else
	STORAGE_CANDIDATE=""
fi
[ -n "${STORAGE_CANDIDATE}" ] && [ -d "${STORAGE_CANDIDATE}" ] &&
	STORAGE_DIR="${STORAGE_CANDIDATE}"
if [ -n "${STORAGE_DIR}" ]; then
	if [ -d "${STORAGE_DIR}/${ATTACH}" ]; then
		info "storage dir ${STORAGE_DIR}/${ATTACH} exists — named for the ATTACHMENT"
		if ! find "${STORAGE_DIR}/${ATTACH}" -maxdepth 1 -mindepth 1 \
			-exec basename {} \; | sed 's/^/      /'; then
			die "could not inspect storage directory ${STORAGE_DIR}/${ATTACH}"
		fi
		STORAGE_LIST="${WORK}/storage-files.txt"
		if ! find "${STORAGE_DIR}/${ATTACH}" -maxdepth 1 -type f -print >"${STORAGE_LIST}"; then
			die "could not list files in ${STORAGE_DIR}/${ATTACH}"
		fi
		STORAGE_MATCH=0
		while IFS= read -r STORAGE_FILE; do
			if [ "${HASHER}" = "md5" ]; then
				STORAGE_MD5="$(md5 -q "${STORAGE_FILE}")" ||
					die "could not hash storage file ${STORAGE_FILE}"
			else
				STORAGE_MD5="$(md5sum "${STORAGE_FILE}" | cut -d' ' -f1)" ||
					die "could not hash storage file ${STORAGE_FILE}"
			fi
			if [ "${STORAGE_MD5}" = "${PROBE_MD5}" ]; then
				STORAGE_MATCH=1
				break
			fi
		done <"${STORAGE_LIST}"
		if [ "${STORAGE_MATCH}" != "1" ]; then
			warn "storage directory ${STORAGE_DIR}/${ATTACH} does not contain this run's file"
			FAILED=1
			INCOMPLETE=1
		fi
	else
		warn "no storage directory named ${ATTACH}; the desktop may not have"
		warn "written it yet, or your data directory is elsewhere"
		FAILED=1
		INCOMPLETE=1
	fi
	if [ -d "${STORAGE_DIR}/${TEMP_PARENT}" ]; then
		warn "a storage directory exists for the TEMPORARY PARENT key — the"
		warn "naming invariant this route depends on does not hold"
		FAILED=1
	fi
else
	warn "could not locate the active Zotero storage directory"
	FAILED=1
	INCOMPLETE=1
fi

if [ "${FAILED}" != "0" ]; then
	if [ "${INCOMPLETE:-0}" = "1" ]; then
		warn "Server-side verification INCOMPLETE; see above"
	else
		warn "server-side verification failed; see above"
	fi
	cleanup_hint
	exit 3
fi
info "Server-side verification completed. Cleaning up the probe objects next."

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
ATTACH_TRASHED=0
if zotio --agent items delete "${ATTACH}" --yes \
	>"${WORK}/trash-attach.json" 2>"${WORK}/trash-attach.err"; then
	if ATTACH_DELETED="$(curl -fsS -H "Zotero-API-Key: ${ZOTERO_API_KEY}" \
		"${API}/items/${ATTACH}" | jq -r '.data.deleted // 0')" &&
		[ "${ATTACH_DELETED}" = "1" ]; then
		ATTACH_TRASHED=1
	else
		warn "could not verify the attachment is trashed after delete"
		FAILED=$((FAILED + 1))
	fi
else
	warn "trashing the attachment failed; see ${WORK}/trash-attach.err"
	FAILED=$((FAILED + 1))
fi

# Never trash a parent after an attachment cleanup failure. That would hide a
# live attachment beneath a trashed parent, which Zotero does not cascade.
if [ "${ATTACH_TRASHED}" = "1" ]; then
	if zotio --agent items delete "${RECEIVER_B}" --yes \
		>"${WORK}/trash-b.json" 2>"${WORK}/trash-b.err"; then
		if RECEIVER_DELETED="$(curl -fsS -H "Zotero-API-Key: ${ZOTERO_API_KEY}" \
			"${API}/items/${RECEIVER_B}" | jq -r '.data.deleted // 0')" &&
			[ "${RECEIVER_DELETED}" = "1" ]; then
			:
		else
			warn "could not verify the receiving item is trashed after delete"
			FAILED=$((FAILED + 1))
		fi
	else
		warn "trashing the receiving item failed; see ${WORK}/trash-b.err"
		FAILED=$((FAILED + 1))
	fi
else
	warn "did not trash the receiving item because the attachment may still be live"
	FAILED=$((FAILED + 1))
fi

if [ "${FAILED}" != "0" ]; then
	warn "cleanup INCOMPLETE; the recovery commands below name every recorded object"
	cleanup_hint
	exit 1
fi

if [ "${PROBE_YES:-}" = "1" ]; then
	bold "Probe complete with manual checks SKIPPED"
else
	bold "Probe PASSED"
fi
info "Everything the probe created is now in your trash."
info "Empty the trash in Zotero when you are satisfied, and sync once more so"
info "the WebDAV files are removed too."
info ""
info "Verify both created live objects are trashed with independent GETs:"
info "  curl -fsS -H \"Zotero-API-Key: \$ZOTERO_API_KEY\" ${API}/items/${ATTACH} | jq -e '.data.deleted == 1'"
info "  curl -fsS -H \"Zotero-API-Key: \$ZOTERO_API_KEY\" ${API}/items/${RECEIVER_B} | jq -e '.data.deleted == 1'"
info ""
info "Keys, for the record:"
cat "${STATE}"
info ""
info "Working directory kept for inspection: ${WORK}"
