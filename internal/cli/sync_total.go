// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// errTotalResultsChanged marks a pass whose Total-Results moved between pages:
// the library changed under the walk, so its offsets no longer describe one
// snapshot. Tail restarts the walk on it; sync refuses the pass.
var errTotalResultsChanged = errors.New("Total-Results changed between pages")

// parseTotalResults reads a Zotero Total-Results header: the number of objects
// the whole query matches, independent of limit and start. present is false for
// an absent header, which is missing evidence rather than an error; err reports
// a header that is present but unusable (non-numeric or negative), which is
// evidence that the response cannot be trusted.
func parseTotalResults(header string) (total int, present bool, err error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, false, nil
	}
	n, perr := strconv.Atoi(header)
	if perr != nil || n < 0 {
		return 0, true, fmt.Errorf("invalid Total-Results header %q", header)
	}
	return n, true, nil
}

// pageTotals judges the completeness of one paginated pass from the
// Total-Results header of each page.
//
// A short page is the pass's natural end only if the server's own count agrees.
// A faulty API or proxy can answer 200 with a well-formed but partial array;
// reading that as the end of the listing makes everything it omitted look
// deleted (full-pass sweep) or unchanged (version checkpoint). The header is
// the one piece of completeness evidence the response carries, so every page's
// header is checked against the offsets actually received:
//
//   - a malformed header is an error;
//   - the header is all-or-nothing across a pass, because a page that drops it
//     after others carried it is a different server answering;
//   - the total must not change between pages, because a moving total means the
//     pass is not one consistent snapshot and offsets may have shifted;
//   - a page must not run past the total;
//   - a pass that ends must end exactly at the total.
//
// A pass whose pages all lack the header proves nothing either way. Callers
// that need the evidence before a destructive step read verified().
type pageTotals struct {
	total   int
	known   bool // at least one page carried a usable Total-Results
	missing bool // at least one page carried none
}

// observe records one page that began at offset start and returned n objects.
func (p *pageTotals) observe(header string, start, n int) error {
	total, present, err := parseTotalResults(header)
	if err != nil {
		return err
	}
	if !present {
		if p.known {
			return fmt.Errorf("page at offset %d carries no Total-Results although earlier pages did", start)
		}
		p.missing = true
		return nil
	}
	if p.missing {
		return fmt.Errorf("page at offset %d carries Total-Results although earlier pages did not", start)
	}
	if p.known && total != p.total {
		return fmt.Errorf("%w: from %d to %d", errTotalResultsChanged, p.total, total)
	}
	p.known, p.total = true, total
	if start+n > total {
		return fmt.Errorf("page at offset %d returned %d object(s), past Total-Results %d", start, n, total)
	}
	return nil
}

// finish checks a pass that ended naturally after receiving end objects.
func (p *pageTotals) finish(end int) error {
	if p.known && end != p.total {
		return fmt.Errorf("listing ended after %d object(s) but Total-Results is %d", end, p.total)
	}
	return nil
}

// verified reports whether every page of the pass carried a Total-Results
// header and all of them agreed with the objects received.
func (p *pageTotals) verified() bool { return p.known && !p.missing }
