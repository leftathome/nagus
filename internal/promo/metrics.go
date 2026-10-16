package promo

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/pipeline"
	"github.com/leftathome/nagus/internal/sanitize"
)

// Message outcomes (nagus_promo_messages_total).
const (
	MsgProcessed = "processed" // every image reached a final outcome this poll
	MsgCached    = "cached"    // already processed; results reused
	MsgDeferred  = "deferred"  // budget or a transient failure left images for a later poll
	MsgDisabled  = "disabled"  // vision is not configured
)

// MessageOutcomes in metric order.
var MessageOutcomes = []string{MsgProcessed, MsgCached, MsgDeferred, MsgDisabled}

// Image stages (nagus_promo_images_total), in pipeline order.
const (
	StageSelected = "selected"
	StageFetched  = "fetched"
	StageRead     = "read"
	StageAgreed   = "agreed"
	StageKept     = "kept"
)

// Stages in metric order.
var Stages = []string{StageSelected, StageFetched, StageRead, StageAgreed, StageKept}

// More drop reasons.
const (
	DropDealInvalid      = "deal_invalid"
	DropSenderNotProfile = "sender_not_in_profile"
	DropVisionFailed     = "vision_failed"
	DropGateRefused      = "gate_refused"
	DropNotInCategory    = "not_in_category"
	DropExtractFailed    = "extract_failed"
)

// DropReasons is the closed label set of nagus_promo_drops_total.
var DropReasons = []string{
	SkipImageHost, SkipHidden, SkipTiny, SkipNav, SkipNoItem, SkipNoLink, SkipDuplicate, SkipOverCap, SkipBadImgURL, SkipNoImageSrc,
	DropFetchFailed, DropTooLarge, DropNotImage, DropSmallImage, DropImgRedirect, DropImgHTTPError,
	DropVisionFailed, DropNoProduct, DropMultiProduct, DropBadPrice, DropPriceDisagree, DropTitleDisagree, DropCurrency, DropPriceSanity, DropNoTitle,
	DropLinkOffsite, DropHTTPDowngrade, DropRedirectLoop, DropTooManyHops, DropLinkUnresolved, DropLinkFailed, DropBadItemNumber,
	DropLinkTitleMismatch, DropDealInvalid, DropSenderNotProfile,
	DropGateRefused, DropNotInCategory, DropExtractFailed,
}

// Metrics are the promo counters, per source. Safe for concurrent use.
type Metrics struct {
	mu       sync.Mutex
	sources  []string
	messages map[[2]string]int64
	images   map[[2]string]int64
	drops    map[[2]string]int64
	// ingestSeen counts an ingest-side drop once per (source, key): every
	// poll re-ingests the same cached products.
	ingestSeen map[string]time.Time
	budget     *Budget
	enabled    bool
	Now        func() time.Time
}

// NewMetrics returns empty counters reporting budget.
func NewMetrics(budget *Budget, enabled bool) *Metrics {
	return &Metrics{
		messages: map[[2]string]int64{}, images: map[[2]string]int64{}, drops: map[[2]string]int64{},
		ingestSeen: map[string]time.Time{}, budget: budget, enabled: enabled, Now: time.Now,
	}
}

// Register adds a source so its series are present at zero.
func (m *Metrics) Register(source string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !slices.Contains(m.sources, source) {
		m.sources = append(m.sources, source)
	}
}

func (m *Metrics) message(source, outcome string) {
	m.mu.Lock()
	m.messages[[2]string{source, outcome}]++
	m.mu.Unlock()
}

func (m *Metrics) image(source, stage string, n int) {
	m.mu.Lock()
	m.images[[2]string{source, stage}] += int64(n)
	m.mu.Unlock()
}

func (m *Metrics) drop(source, reason string, n int) {
	m.mu.Lock()
	m.drops[[2]string{source, reason}] += int64(n)
	m.mu.Unlock()
}

// Drops returns a source's drop count for a reason (tests, logs).
func (m *Metrics) Drops(source, reason string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.drops[[2]string{source, reason}]
}

// Images returns a source's count for a stage.
func (m *Metrics) Images(source, stage string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.images[[2]string{source, stage}]
}

// Messages returns a source's count for a message outcome.
func (m *Metrics) Messages(source, outcome string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.messages[[2]string{source, outcome}]
}

// ingestSeenTTL bounds how long a counted ingest drop is remembered.
const ingestSeenTTL = 30 * 24 * time.Hour

// ObserveIngest counts a promo source's ingest-side drops (the glovebox gate
// and the category extractor), once per product. It is the ingester's
// AfterIngest hook.
func (m *Metrics) ObserveIngest(source string, res pipeline.IngestResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.Now()
	for k, at := range m.ingestSeen {
		if now.Sub(at) > ingestSeenTTL {
			delete(m.ingestSeen, k)
		}
	}
	for _, s := range res.Skips {
		var reason string
		switch s.Stage {
		case "sanitize":
			if !errors.Is(s.Err, sanitize.ErrQuarantined) {
				continue // the gate was unavailable: retried next poll
			}
			reason = DropGateRefused
		case "extract":
			reason = DropExtractFailed
			if errors.Is(s.Err, listing.ErrNotInCategory) {
				reason = DropNotInCategory
			}
		default:
			continue
		}
		k := source + "\x00" + reason + "\x00" + s.SourceKey
		if _, ok := m.ingestSeen[k]; ok {
			m.ingestSeen[k] = now
			continue
		}
		if len(m.ingestSeen) >= 50000 {
			m.ingestSeen = map[string]time.Time{}
		}
		m.ingestSeen[k] = now
		m.drops[[2]string{source, reason}]++
	}
}

// WriteMetrics renders every series of the bounded label sets, zeros
// included.
func (m *Metrics) WriteMetrics(out io.Writer) {
	var buf bytes.Buffer
	m.render(&buf)
	_, _ = out.Write(buf.Bytes())
}

func (m *Metrics) render(w *bytes.Buffer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	en := 0
	if m.enabled {
		en = 1
	}
	fmt.Fprintf(w, "# HELP nagus_promo_vision_enabled 1 when NAGUS_VISION_BASE_URL, _API_KEY and _MODEL are all set (promo-vision-v1 sources read images).\n")
	fmt.Fprintf(w, "# TYPE nagus_promo_vision_enabled gauge\n")
	fmt.Fprintf(w, "nagus_promo_vision_enabled %d\n", en)
	fmt.Fprintf(w, "# HELP nagus_promo_messages_total Retailer promo messages handled by promo-vision-v1, by outcome, counted per poll.\n")
	fmt.Fprintf(w, "# TYPE nagus_promo_messages_total counter\n")
	for _, s := range m.sources {
		for _, o := range MessageOutcomes {
			fmt.Fprintf(w, "nagus_promo_messages_total{source=%q,outcome=%q} %d\n", s, o, m.messages[[2]string{s, o}])
		}
	}
	fmt.Fprintf(w, "# HELP nagus_promo_images_total Promo images that reached each stage (selected, fetched, read twice, agreed, kept as a deal), counted once per image.\n")
	fmt.Fprintf(w, "# TYPE nagus_promo_images_total counter\n")
	for _, s := range m.sources {
		for _, st := range Stages {
			fmt.Fprintf(w, "nagus_promo_images_total{source=%q,stage=%q} %d\n", s, st, m.images[[2]string{s, st}])
		}
	}
	fmt.Fprintf(w, "# HELP nagus_promo_drops_total Promo images or products dropped, by reason (selection, fetch, agreement guard, link resolution, deal validation, gate, category), counted once.\n")
	fmt.Fprintf(w, "# TYPE nagus_promo_drops_total counter\n")
	for _, s := range m.sources {
		for _, r := range DropReasons {
			fmt.Fprintf(w, "nagus_promo_drops_total{source=%q,reason=%q} %d\n", s, r, m.drops[[2]string{s, r}])
		}
	}
	if m.budget == nil {
		return
	}
	st := m.budget.Stats()
	fmt.Fprintf(w, "# HELP nagus_promo_vision_calls_total Vision model calls by result.\n")
	fmt.Fprintf(w, "# TYPE nagus_promo_vision_calls_total counter\n")
	fmt.Fprintf(w, "nagus_promo_vision_calls_total{result=\"ok\"} %d\n", st.CallsOK)
	fmt.Fprintf(w, "nagus_promo_vision_calls_total{result=\"error\"} %d\n", st.CallsErr)
	fmt.Fprintf(w, "# HELP nagus_promo_vision_calls_today Vision calls reserved today (UTC), all promo sources.\n")
	fmt.Fprintf(w, "# TYPE nagus_promo_vision_calls_today gauge\n")
	fmt.Fprintf(w, "nagus_promo_vision_calls_today %d\n", st.UsedToday)
	fmt.Fprintf(w, "# HELP nagus_promo_vision_calls_per_day_limit Configured daily vision call budget (NAGUS_VISION_MAX_CALLS_PER_DAY).\n")
	fmt.Fprintf(w, "# TYPE nagus_promo_vision_calls_per_day_limit gauge\n")
	fmt.Fprintf(w, "nagus_promo_vision_calls_per_day_limit %d\n", st.PerDay)
	fmt.Fprintf(w, "# HELP nagus_promo_vision_budget_exhausted_total Image reads deferred because a vision budget ran out, by scope.\n")
	fmt.Fprintf(w, "# TYPE nagus_promo_vision_budget_exhausted_total counter\n")
	for _, sc := range []string{"poll", "day"} {
		fmt.Fprintf(w, "nagus_promo_vision_budget_exhausted_total{scope=%q} %d\n", sc, st.Exhausted[sc])
	}
}
