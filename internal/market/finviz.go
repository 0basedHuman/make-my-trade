// internal/market/finviz.go
//
// WHAT: Scrapes finviz.com/quote.ashx?t=TICKER for short interest data and
//       recent news headlines. No API key required.
//
// WHY:  Finviz aggregates short float %, short ratio (days to cover), and
//       ~20 recent news headlines per ticker. Short interest direction
//       matters: high short float on a bearish setup = crowded short =
//       either strong confirmation OR squeeze risk. Headlines feed Claude
//       context for confidence scoring.
//
// RATE LIMIT: Be conservative — 1 request per ticker with ≥500ms spacing.
//             Aggressive scraping will result in Cloudflare blocks.
//
// WHAT BREAKS: Finviz periodically changes their HTML structure. If Short Float
//              returns 0.0, check the snapshot table selectors below.

package market

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const finvizBaseURL = "https://finviz.com/quote.ashx?t="

// FinvizSnapshot holds short interest metrics and recent news for one ticker.
type FinvizSnapshot struct {
	Ticker        string
	ShortFloatPct float64  // e.g. 5.23 (percent)
	ShortRatio    float64  // days to cover
	Headlines     []string // up to 15 most recent news headlines
}

var (
	// Finviz snapshot table: label cell immediately precedes value cell.
	// Pattern: >LABEL</td>\s*<td[^>]*><b>VALUE</b>
	reFinvizStat = regexp.MustCompile(`>([^<]+)</td>\s*<td[^>]*><b>([^<]+)</b>`)

	// Alternate pattern without <b> wrapper
	reFinvizStatAlt = regexp.MustCompile(`>([^<]+)</td>\s*<td[^>]*>([^<\s][^<]*)</td>`)

	// News table anchor text
	reNewsAnchor = regexp.MustCompile(`<a\s+[^>]*class="[^"]*news-link[^"]*"[^>]*>([^<]+)</a>`)
)

// FetchFinvizSnapshot scrapes finviz.com for one ticker.
// Returns a zero-value snapshot on any fetch or parse error.
func FetchFinvizSnapshot(ticker string) (FinvizSnapshot, error) {
	snap := FinvizSnapshot{Ticker: ticker}

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", finvizBaseURL+ticker, nil)
	if err != nil {
		return snap, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")

	resp, err := client.Do(req)
	if err != nil {
		return snap, fmt.Errorf("finviz fetch %s: %w", ticker, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return snap, fmt.Errorf("finviz %s HTTP %d", ticker, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return snap, fmt.Errorf("finviz read %s: %w", ticker, err)
	}
	html := string(body)

	// Parse snapshot stats: scan for label→value pairs
	snap.ShortFloatPct = extractFinvizStat(html, "Short Float")
	snap.ShortRatio = extractFinvizStat(html, "Short Ratio")

	// Parse news headlines
	snap.Headlines = extractFinvizHeadlines(html)

	return snap, nil
}

// extractFinvizStat finds a stat label and returns the first <b>VALUE</b> within
// 600 characters after it. Finviz renders stats as label <td> → value <td> pairs;
// the value is always wrapped in <b> tags inside the next cell.
func extractFinvizStat(html, label string) float64 {
	idx := strings.Index(html, label)
	if idx < 0 {
		return 0
	}
	window := html[idx:min(idx+600, len(html))]
	reBold := regexp.MustCompile(`<b[^>]*>([0-9.,]+%?)</b>`)
	if m := reBold.FindStringSubmatch(window); len(m) > 1 {
		return parseFinvizFloat(m[1])
	}
	return 0
}

// parseFinvizFloat converts "5.23%" or "3.45" to float64.
func parseFinvizFloat(s string) float64 {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "%")
	s = strings.ReplaceAll(s, ",", "")
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// extractFinvizHeadlines pulls news anchor text from the finviz news table.
func extractFinvizHeadlines(html string) []string {
	// News table starts with id="news-table"
	tableStart := strings.Index(html, `id="news-table"`)
	if tableStart < 0 {
		// Fallback: look for news-link anchors anywhere
		return extractAnchors(html, `news-link`, 15)
	}
	// Find the closing </table>
	tableEnd := strings.Index(html[tableStart:], "</table>")
	if tableEnd < 0 {
		tableEnd = min(len(html)-tableStart, 20000)
	} else {
		tableEnd += tableStart
	}
	section := html[tableStart:tableEnd]
	return extractAnchors(section, "", 15)
}

// extractAnchors pulls text content from <a> tags matching an optional class substring.
func extractAnchors(html, classHint string, max int) []string {
	// Match <a href="...">Headline text</a>
	reA := regexp.MustCompile(`<a\s[^>]*href="[^"]*"[^>]*>([^<]{10,200})</a>`)
	matches := reA.FindAllStringSubmatch(html, -1)
	var out []string
	for _, m := range matches {
		if len(out) >= max {
			break
		}
		text := strings.TrimSpace(m[1])
		if text == "" || len(text) < 10 {
			continue
		}
		// Skip nav/UI links
		if strings.ContainsAny(text, "<>{}") {
			continue
		}
		if classHint != "" && !strings.Contains(m[0], classHint) {
			continue
		}
		out = append(out, text)
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
