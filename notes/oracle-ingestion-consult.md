## 1. Recommendation: choose **B — companion**, but make it a **first-class companion with a thin integrated facade**

Your instinct is right, but I would sharpen it:

**Do not make authenticated institutional acquisition a normal in-core `import`/`enrich` feature.** Make the browser/auth/fetch engine a separate binary/runtime, but let the main CLI own the *planning, review, policy, manifest, and write-back handoff*. In other words:

```text
zotio                    = trusted Zotero library operator
zotio-acquire             = risky authenticated acquisition runtime
zotio acquire ...         = core facade that calls/coordinates zotio-acquire if installed
```

That gives you the UX of one verb without poisoning the core architecture.

The core tool’s identity is already very explicit: local-fast reads, preview-first Web API writes, provenance, bounded agent context, and no in-tool LLM calls. The roadmap frames it as a “trust-and-automation layer for Zotero,” not a browser automation platform, and says the import path is reviewable ingest over `import scan → resolve → apply`.   The repo invariants are also crisp: reads target Zotero’s local API / mirror; local API writes are unsupported; mutations auto-route to the Zotero Web API with an API key; nothing writes the local DB directly.  Zotero’s current Local API docs still confirm that the local API is offline/fast but accepts only `GET` and has no auth. ([Zotero][1])

Institutional acquisition violates almost every aesthetic and operational property that makes the core good: it is browserful, sessionful, non-deterministic, domain-specific, brittle, and legally contextual. That is not a reason not to build it; it is a reason to quarantine it.

My concrete recommendation is **B+**:

**Integrate:**

* Target selection: “which Zotero items are missing PDFs?”
* Review plan: item keys, DOI, publisher, candidate landing page, acquisition route, expected action.
* Policy gates: per-domain caps, explicit user authorization, dry-run by default.
* Handoff: produce or consume a manifest compatible with `import apply`.
* Write-back: core remains the only component that creates Zotero attachments, journals mutations, and applies preview/undo semantics where available.

**Separate:**

* SSO/session lifecycle.
* Browser automation.
* Publisher/proxy adapters.
* PDF discovery/download.
* Cookie/session storage.
* ToS/domain policy files.

That split also fits your existing machinery: `import apply` already validates a reviewed manifest and only supports `none` or `linked-file`, with `upload` explicitly refused for now.  The manifest is already the editable contract between `import resolve` and `import apply`, including absolute PDF paths, matched item keys, action, identifiers, and resolved item payloads. 

## 2. The three options

| Option                                         |     Verdict | UX                                                                                                                                                                                            | Security                                                                                                                                                       | Maintenance                                                                                                               | Legal / ToS                                                                                                                                     | Dependency / identity impact                                                                                                        |
| ---------------------------------------------- | ----------: | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------- |
| **A. Fully integrated in core CLI**            |      **No** | Best surface: `zotio acquire <doi>` works immediately. Easy for agents.                                                                                                                       | Worst blast radius: browser cookies, institutional SSO sessions, publisher pages, and Zotero write credentials all live in one trusted binary/config boundary. | Worst: every publisher DOM break becomes a core release problem. Browser runtime becomes part of the main support matrix. | Worst: the main project becomes visibly a paywalled bulk-acquisition tool, which is not the same ethical/legal posture as managing a library.   | Breaks the “single static Go binary, deterministic, offline-first” promise. Also makes Homebrew packaging heavier and more fragile. |
| **B. Companion in same project/module family** |     **Yes** | Good if exposed through a core facade: `zotio acquire plan/apply`, with `zotio-acquire` installed separately. Some extra setup, but not fragmented if config/profile discovery is shared.       | Best balance: browser/session secrets are isolated; core retains Zotero write authority and journal guarantees.                                                | Manageable: acquisition adapters can rev independently, even be experimental or institution-specific.                     | Best balance: you can ship strong policy gates, per-domain caps, and “personal entitled use only” warnings without redefining the core product. | Preserves core identity. Heavy browser deps live in the companion formula/binary.                                                   |
| **C. Entirely standalone acquisition product** | Maybe later | Weakest Zotero UX unless it reimplements scope, mirror, health, manifest, write-back, config, and MCP.                                                                                        | Cleanest isolation from the core, but only if it does **not** share tokens/config.                                                                             | Potentially cleaner if the tool grows beyond Zotero or becomes a general “institutional PDF fetcher.”                     | Best if you want legal separation from the Zotero automation project.                                                                           | Duplicates too much for a private Zotero-first tool. More product than you need today.                                              |

I would only choose **C** if you intend to publish this as a general-purpose “institutional PDF acquisition” tool for many reference managers. For your stated use case — one real Zotero library, one CLI ecosystem, one safe writer — **C is over-separation**.

I would only choose **A** if the acquisition surface stabilizes into documented APIs, which is unlikely. Even Zotero’s own browser save flow is translator/site-dependent; Zotero recommends its Connector for high-quality web saving and notes it can download accessible PDFs, but also warns that large-scale saving from scholarly databases can trigger lockouts and recommends batch export where possible. ([Zotero][2]) That is exactly the class of risk you do not want to embed as a normal core command.

## 3. Proposed institutional-auth architecture

### Component boundary

Use a two-process design:

```text
zotio
  - reads Zotero local API / mirror
  - selects missing-PDF items
  - builds acquisition plan
  - validates policy
  - previews attach operations
  - writes to Zotero via Web API only
  - journals applied mutations

zotio-acquire
  - owns browser profiles and sessions
  - performs institutional login flows
  - probes article landing pages
  - downloads entitled PDFs
  - writes files to a local spool
  - emits acquisition manifest entries
```

The acquisition companion should not know how to mutate Zotero, except maybe to ask core for item metadata through a stable JSON command. It should emit something like:

```json
{
  "schema_version": 1,
  "source": "zotio-acquire",
  "entries": [
    {
      "item_key": "ABCD1234",
      "doi": "10.xxxx/yyyy",
      "title": "Paper title",
      "publisher": "Elsevier",
      "landing_url": "https://doi.org/...",
      "access_route": "institution_proxy",
      "auth_profile": "uwa",
      "downloaded_path": "/Users/me/.local/share/zotio-acquire/spool/ABCD1234.pdf",
      "sha256": "...",
      "content_type": "application/pdf",
      "bytes": 1234567,
      "action": "attach",
      "policy": {
        "user_confirmed_entitled_access": true,
        "rate_limit_bucket": "elsevier.com",
        "terms_status": "user-reviewed"
      }
    }
  ]
}
```

Then core converts that to the existing `import apply` shape or extends the manifest schema with an `item_key`/`path` attach entry. Core already has the mutation engine and preview/apply semantics; do not duplicate them.

### Auth/session model

Support three auth profiles, in this order:

1. **Browser-mediated SSO profile.**
   `zotio-acquire auth login <profile>` opens a headed browser. The user logs in through EZproxy, OpenAthens, Shibboleth/SAML, Entra, Duo, Okta, or whatever the institution uses. The tool never asks for username, password, or MFA code.

2. **Proxy-prefix profile.**
   For institutions that use EZproxy-style rewritten URLs, store only the proxy base/prefix and session cookies created by the browser login. EZproxy officially supports SAML-style authentication integrations, including Shibboleth, ADFS, Microsoft Entra ID, and OpenAthens, but the setup is metadata/certificate/IdP-specific rather than a single generic API. ([OCLC Support][3])

3. **OpenAthens / federated profile.**
   Treat OpenAthens and Shibboleth as browser login flows, not CLI password flows. OpenAthens SAML resources depend on application metadata and attribute-release policy, which reinforces that this is a federation/session problem, not something your CLI should pretend to “log into” directly. ([docs.openathens.net][4])

Store **session state, not passwords**. Use OS keychain where possible, keep browser-profile directories `0700`, encrypt cookie/session state at rest, and expose `auth status`, `auth logout`, and `auth purge`. Session cookies are credentials; treat them as more sensitive than a Zotero API key because they may imply broad institutional access.

### Browser automation

Use a real browser automation layer in the companion. I would pick **Playwright** for the first version despite the heavier dependency, because it gives you reliable browser contexts, cross-browser support, auth-state reuse, tracing, and headed/headless transitions. Playwright explicitly supports browser automation across Chromium, Firefox, and WebKit and supports saving authentication state for reuse in isolated contexts. ([Playwright][5])

Keep the rule strict:

* **Headed browser for login and MFA.**
* **Headless only after an authenticated session exists.**
* **No CAPTCHA solving.**
* **No MFA interception.**
* **No password storage.**
* **No bypass of access controls.**
* **No automatic use of the user’s normal browser cookies unless they explicitly import a profile.**

Use automation for boring navigation and download capture, not for defeating access gates.

### Acquisition flow

A safe command flow could be:

```bash
zotio acquire plan --scope 'collection:XYZ' --missing-pdf --limit 20 > acquire-plan.json
zotio-acquire fetch acquire-plan.json --profile uwa --max-per-domain 3 --out ./pdf-spool > fetched.json
zotio import apply fetched.json --attach-mode linked-file --dry-run
zotio import apply fetched.json --attach-mode linked-file --yes
```

Then add the UX sugar:

```bash
zotio acquire run --scope 'tag:needs-pdf' --profile uwa --limit 10
```

That command can internally call `zotio-acquire`, but the surface still behaves like the rest of the tool: preflight, plan, dry-run, preview, apply.

For stored Zotero file upload, keep it out of the MVP. Zotero’s Web API supports file uploads, but it is a multi-step protocol: create an attachment item, request upload authorization, POST the file, then register it; the API also has quota, permission, conflict, precondition, and rate-limit failure modes. ([Zotero][6]) ([Zotero][6]) Your roadmap already correctly defers `upload` and starts with `linked-file`, noting that linked files are lower risk but do not sync, do not work for group libraries, and are not mobile-friendly.  Zotero’s own docs strongly recommend stored files for seamless sync, while linked files remain local paths with limitations. ([Zotero][7])

### MCP / agent surface

Do **not** expose raw browser automation over MCP. Expose only high-level, bounded acquisition operations:

```text
acquire.plan
acquire.fetch_next_batch
acquire.status
acquire.cancel
acquire.apply_manifest
```

When auth is required, return a precondition envelope:

```json
{
  "ok": false,
  "status": "precondition_unmet",
  "precondition": "institutional_browser_session",
  "remediation": [
    {
      "action": "user_login",
      "command": "zotio-acquire auth login uwa"
    }
  ],
  "retry_after_remediation": true
}
```

That matches your existing precondition philosophy: capabilities that need setup should refuse loudly and machine-readably, rather than silently degrading. 

## 4. Biggest risks / what you may be underestimating

### 1. “Entitled to read” is not the same as “allowed to automate bulk download”

This is the biggest product risk. Your personal moral intuition may be right — you are collecting PDFs you can access — but publisher, database, and university acceptable-use terms may still prohibit automated download, systematic harvesting, or unusual request patterns. Zotero itself warns that frequent or large-scale use of browser saving from scholarly databases can cause lockouts. ([Zotero][2])

Design implication: build **rate-limited acquisition**, not “bulk download.” Default to small batches. Require per-run confirmation. Keep per-domain caps. Emit a report the user can audit.

### 2. The browser session is a new class of secret

A Zotero API key is scoped to Zotero. An institutional browser session can unlock publisher platforms, library databases, and possibly unrelated university services depending on SSO scope. Do not store reusable passwords, do not print cookies in JSON, do not include signed download URLs in logs, and do not let agents inspect browser state.

### 3. Publisher adapters will rot constantly

Do not build a “universal publisher scraper.” Build a generic resolver plus a small adapter registry:

```text
generic DOI landing page
generic citation_pdf_url / meta tag extraction
generic “download link with PDF MIME” detector
publisher-specific adapter only when personally needed
```

Every adapter should have: domain allowlist, selectors, expected MIME/type checks, test fixture, last-validated date, and failure mode. Broken adapter means “skip with reason,” not “guess.”

### 4. You can easily attach the wrong file

The PDF link on a page might be a supplement, accepted manuscript, preview, HTML wrapper, or CAPTCHA/denial page saved as `.pdf`. The companion should verify:

* HTTP content type.
* `%PDF-` header.
* minimum file size.
* SHA-256.
* maybe DOI/title text extraction if feasible.
* “downloaded from landing DOI X for Zotero item Y” provenance.

For uncertain matches, emit `needs_review`, not `attach`.

### 5. The write-back path is not done until stored upload is done

Linked files are fine for a private desktop-first workflow, but the moment you want Zotero sync/mobile/group parity, you need stored-file upload. That should be a **core capability**, not an acquisition capability, because it is Zotero mutation logic and belongs under the existing journal/preview/write-routing system. Zotero’s API requires write access for write methods and version/precondition handling; conflicts return `412`, which aligns with your existing stale-local-read concern. ([Zotero][8])

### 6. Public distribution changes the risk profile

For a private tool, a companion is pragmatic. For a public Homebrew formula, it is a support and policy lightning rod. I would not pitch it as “bulk paywall downloader.” I would document it as:

> “Personal, rate-limited, review-first acquisition of PDFs you can access through your institution, with no credential capture and no access-control bypass.”

Also consider keeping `zotio-acquire` out of the default install. Let users opt in.

### 7. Do not merge this into `items enrich --missing-pdf`

That command already has a clean meaning: metadata providers and open-access PDF links. The code currently resolves missing PDFs through Unpaywall and creates a linked URL attachment for open-access PDFs.   Authenticated acquisition is not enrichment; it is a separate trust domain.

## Build order I would use

First, add a **core acquisition planner**:

```bash
zotio acquire plan --scope ... --missing-pdf --json
```

It should produce item keys, DOI, title, URL/DOI landing candidates, current attachment state, and policy defaults. No browser yet.

Second, define `acquire_manifest_v1` and make core able to preview/apply attach entries from local PDF paths.

Third, build `zotio-acquire auth login` with one profile and one generic fetch path: DOI landing page → institutional session → PDF candidate → verified PDF file in spool.

Fourth, add a single institution/proxy profile and only the publishers you actually need. Do not chase broad coverage until the full loop is safe.

Fifth, add stored-file upload in core, behind `--attach-mode upload`, after you are happy with linked-file behavior.

## 5. Naming verdict

**The project name is now `zotio`; use `zotio` for the core command and `zotio-acquire` for any acquisition companion.** The earlier `zoteria` candidate was memorable and Zotero-adjacent, but it carried trademark-proximity and collision risks. Zotero’s trademark policy says “Zotero” is a registered trademark and warns against confusing unaffiliated product use, so avoid Zotero logos, “official” language, and maybe use “for Zotero” rather than “Zotero CLI” in packaging. ([Zotero][9]) I also found current non-package uses of “Zoteria,” including an Australian insurance group and a Vodafone Foundation app, so it is not globally unique. ([Zoteria Insurance Group][10]) ([Vodafone Careers][11]) Your note that `zot` is taken is correct: Homebrew currently has `zot` as a Go coding-agent harness. ([formulae.brew.sh][12])

[1]: https://www.zotero.org/support/dev/web_api/v3/local_api "Zotero Local API | Zotero Documentation"
[2]: https://www.zotero.org/support/adding_items_to_zotero "Adding Items to Zotero | Zotero Documentation"
[3]: https://help.oclc.org/Library_Management/EZproxy/Authenticate_users/EZproxy_authentication_methods/SAML_authentication "SAML Authentication (including Shibboleth V1/2/3, ADFS, Microsoft Entra ID (Azure), OpenAthens) - OCLC Support"
[4]: https://docs.openathens.net/tpa/sign-in-to-a-generic-application-using-openathens "Sign in to a generic application using OpenAthens"
[5]: https://playwright.dev/ "Fast and reliable end-to-end testing for modern web apps | Playwright"
[6]: https://www.zotero.org/support/dev/web_api/v3/file_upload "Zotero Web API File Uploads | Zotero Documentation"
[7]: https://www.zotero.org/support/attaching_files "Adding Files to your Zotero Library | Zotero Documentation"
[8]: https://www.zotero.org/support/dev/web_api/v3/write_requests "Zotero Web API Write Requests | Zotero Documentation"
[9]: https://www.zotero.org/support/terms/trademark "Usage of Zotero Trademarks | Zotero Documentation"
[10]: https://zoteria.com.au/?utm_source=chatgpt.com "Zoteria Insurance Group: Home"
[11]: https://careers.vodafone.com/life-at-vodafone/projects-stories/tackling-lgbtq-hate-crime-with-zoteria/?utm_source=chatgpt.com "Tackling LGBTQ+ Hate Crime with Zoteria"
[12]: https://formulae.brew.sh/formula/zot "Homebrew Formulae: zot"
