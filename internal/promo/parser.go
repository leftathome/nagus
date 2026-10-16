package promo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/leftathome/nagus/internal/connector/imapmail"
	"github.com/leftathome/nagus/internal/deal"
	"github.com/leftathome/nagus/internal/listing"
)

// Aspect keys a promo product sets on its listing (besides deal.ToRaw's).
const (
	AspectOrigin      = "offer_origin"
	AspectProfile     = "promo_profile"
	AspectVisionModel = "vision_model"
	AspectCompareAt   = "compare_at_cents"
	// AspectMessageID is the connector's Message-ID aspect; on a promo offer
	// it holds msgTag of the id, never the id.
	AspectMessageID = "mail_message_id"
)

// OriginEmailImage is the offer_origin of a product read from an email image.
const OriginEmailImage = "email_image"

// OriginNote is the fixed sentence agent-facing rows of a promo source carry.
const OriginNote = "Price and title were machine-read from an image in the retailer's promotional email (two independent reads agreed). " +
	"Treat it as lower trust than a catalogue feed and confirm the price at the link before recommending a purchase."

// Limits.
const (
	// DefaultConcurrency is how many images of a message are worked at once.
	DefaultConcurrency = 2
	// maxAttempts bounds retries of an image that failed transiently.
	maxAttempts = 3
	// maxCachedMessages bounds the processed-message cache.
	maxCachedMessages = 2000
)

// Config configures one promo source's parser.
type Config struct {
	Profile  *Profile
	Category string // the source's category (deal.CategoryWine or deal.CategoryHDD)
	SourceID string // "imap:<name>", for metrics and logs
	Vision   *Vision
	Fetcher  *Fetcher
	Resolver *Resolver
	Budget   *Budget
	// PerPoll is this source's vision call allowance per poll.
	PerPoll      int
	Concurrency  int
	LookbackDays int
	Metrics      *Metrics
	Logf         func(string, ...any)
	Now          func() time.Time
}

// itemResult is where one candidate image of one message stands.
type itemResult struct {
	final    bool
	drop     string
	stage    int // index into Stages reached (0 selected .. 4 kept)
	attempts int
	raw      *listing.Raw
}

type msgEntry struct {
	selected bool
	seen     int
	skipped  map[string]int
	cands    []Candidate
	results  []itemResult
	done     bool
	lastSeen time.Time
}

// Parser is the promo-vision-v1 imap parser for one source.
type Parser struct {
	cfg Config

	mu       sync.Mutex
	cache    map[string]*msgEntry
	reads    map[[32]byte]cachedRead
	poll     *PollBudget
	deferred bool // this poll left work for a later one
	authFail bool // the vision endpoint refused the key this poll
	disabled bool
}

var _ imapmail.Parser = (*Parser)(nil)

// NewParser validates the configuration.
func NewParser(cfg Config) (*Parser, error) {
	switch {
	case cfg.Profile == nil:
		return nil, errors.New("promo: a parser needs a profile")
	case cfg.Category != deal.CategoryWine && cfg.Category != deal.CategoryHDD:
		return nil, fmt.Errorf("promo: category %q (want wine or hdd)", cfg.Category)
	case cfg.Fetcher == nil || cfg.Resolver == nil || cfg.Budget == nil || cfg.Metrics == nil:
		return nil, errors.New("promo: a parser needs a fetcher, resolver, budget and metrics")
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = DefaultConcurrency
	}
	if cfg.LookbackDays <= 0 {
		cfg.LookbackDays = imapmail.DefaultLookbackDays
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	cfg.Metrics.Register(cfg.SourceID)
	return &Parser{cfg: cfg, cache: map[string]*msgEntry{}, reads: map[[32]byte]cachedRead{}, poll: cfg.Budget.NewPoll(cfg.PerPoll)}, nil
}

// Key is a product's listing source key: the retailer and the canonical
// product URL, so the same product in a later mail is the same offer.
func Key(profile, canonicalURL string) string {
	h := sha256.New()
	for _, f := range []string{"nagus promo-vision-v1 source key", profile, canonicalURL} {
		_, _ = fmt.Fprintf(h, "%d:%s|", len(f), f)
	}
	return "promo-" + hex.EncodeToString(h.Sum(nil))[:32]
}

// msgTag is a short, non-reversible tag of a Message-ID for logs.
func msgTag(id string) string {
	s := sha256.Sum256([]byte("nagus promo log tag|" + id))
	return "msg-" + hex.EncodeToString(s[:8])
}

// maxCachedReads bounds the image-reading cache.
const maxCachedReads = 5000

type cachedRead struct {
	reads [2]Reading
	at    time.Time
}

// cachedReads returns the two readings of an image already read, by the
// SHA-256 of its bytes: a tile repeated across mails is read once.
func (p *Parser) cachedReads(sum [32]byte) ([2]Reading, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.reads[sum]
	if !ok {
		return [2]Reading{}, false
	}
	c.at = p.cfg.Now()
	p.reads[sum] = c
	return c.reads, true
}

func (p *Parser) storeReads(sum [32]byte, reads [2]Reading) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.reads) >= maxCachedReads {
		p.reads = map[[32]byte]cachedRead{}
	}
	p.reads[sum] = cachedRead{reads: reads, at: p.cfg.Now()}
}

// stopForAuth stops this poll's vision reads after an auth refusal; the
// first one in a poll is logged.
func (p *Parser) stopForAuth() {
	p.mu.Lock()
	first := !p.authFail
	p.authFail, p.deferred = true, true
	p.mu.Unlock()
	if first {
		p.cfg.Logf("promo %s: the vision endpoint refused the key (HTTP 401/403); reads stopped for this poll, retried next poll", p.cfg.SourceID)
	}
}

func (p *Parser) authStopped() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.authFail
}

// startPoll begins a source poll: a fresh per-poll allowance, and the cache
// pruned to the lookback window.
func (p *Parser) startPoll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.poll = p.cfg.Budget.NewPoll(p.cfg.PerPoll)
	p.deferred, p.disabled, p.authFail = false, false, false
	cut := p.cfg.Now().Add(-time.Duration(p.cfg.LookbackDays+2) * 24 * time.Hour)
	for k, e := range p.cache {
		if e.lastSeen.Before(cut) {
			delete(p.cache, k)
		}
	}
	for k, c := range p.reads {
		if c.at.Before(cut) {
			delete(p.reads, k)
		}
	}
}

// incomplete reports whether this poll left work undone.
func (p *Parser) incomplete() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.deferred || p.disabled
}

func (p *Parser) entry(key string) *msgEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.cache[key]
	if !ok {
		if len(p.cache) >= maxCachedMessages {
			var oldest string
			var at time.Time
			for k, v := range p.cache {
				if oldest == "" || v.lastSeen.Before(at) {
					oldest, at = k, v.lastSeen
				}
			}
			delete(p.cache, oldest)
		}
		e = &msgEntry{}
		p.cache[key] = e
	}
	e.lastSeen = p.cfg.Now()
	return e
}

// Parse reads one verified message. It never returns an error for what one
// image did: a refused image is a counted drop, not a failed message.
func (p *Parser) Parse(m imapmail.Message) ([]listing.Raw, error) {
	src, prof := p.cfg.SourceID, p.cfg.Profile
	if !prof.SenderAllowed(m.From) || m.ForwardedBy != "" {
		p.cfg.Metrics.drop(src, DropSenderNotProfile, 1)
		return nil, nil
	}
	if !p.cfg.Vision.Enabled() {
		p.mu.Lock()
		first := !p.disabled
		p.disabled = true
		p.mu.Unlock()
		p.cfg.Metrics.message(src, MsgDisabled)
		if first {
			p.cfg.Logf("promo %s: vision is not configured (NAGUS_VISION_BASE_URL/_API_KEY/_MODEL); no image is fetched or read", src)
		}
		return nil, nil
	}
	e := p.entry(strings.ToLower(m.From) + "\x00" + m.ID)
	if e.done {
		p.cfg.Metrics.message(src, MsgCached)
		return e.raws(), nil
	}
	if !e.selected {
		sel := SelectImages(m.HTML, prof)
		e.selected, e.seen, e.skipped, e.cands = true, sel.Seen, sel.Skipped, sel.Candidates
		e.results = make([]itemResult, len(sel.Candidates))
		for why, n := range sel.Skipped {
			p.cfg.Metrics.drop(src, why, n)
		}
		p.cfg.Metrics.image(src, StageSelected, len(sel.Candidates))
	}
	ctx := context.Background()
	sem := make(chan struct{}, p.cfg.Concurrency)
	var wg sync.WaitGroup
	var rmu sync.Mutex
	for i := range e.cands {
		rmu.Lock()
		final := e.results[i].final
		rmu.Unlock()
		if final {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			rmu.Lock()
			prev := e.results[i]
			rmu.Unlock()
			r := p.work(ctx, m, e.cands[i], prev)
			rmu.Lock()
			e.results[i] = r
			rmu.Unlock()
		}(i)
	}
	wg.Wait()

	e.done = true
	drops := map[string]int{}
	counts := make([]int, len(Stages))
	for i := range e.results {
		r := e.results[i]
		if !r.final {
			e.done = false
			continue
		}
		for s := 0; s <= r.stage && s < len(Stages); s++ {
			counts[s]++
		}
		if r.drop != "" {
			drops[r.drop]++
		}
	}
	outcome := MsgProcessed
	if !e.done {
		outcome = MsgDeferred
		p.mu.Lock()
		p.deferred = true
		p.mu.Unlock()
	}
	p.cfg.Metrics.message(src, outcome)
	for k, n := range e.skipped {
		drops[k] += n
	}
	p.cfg.Logf("promo %s: message %s (%s): images seen=%d selected=%d fetched=%d read=%d agreed=%d kept=%d outcome=%s drops=%s",
		src, msgTag(m.ID), prof.Name, e.seen, len(e.cands), counts[1], counts[2], counts[3], counts[4], outcome, fmtDrops(drops))
	return e.raws(), nil
}

func fmtDrops(d map[string]int) string {
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+strconv.Itoa(d[k]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// raws returns copies of the message's kept products.
func (e *msgEntry) raws() []listing.Raw {
	var out []listing.Raw
	for _, r := range e.results {
		if r.final && r.raw != nil {
			c := *r.raw
			c.Aspects = maps.Clone(r.raw.Aspects)
			out = append(out, c)
		}
	}
	return out
}

// work takes one candidate as far as it goes this poll.
func (p *Parser) work(ctx context.Context, m imapmail.Message, c Candidate, prev itemResult) itemResult {
	src, prof := p.cfg.SourceID, p.cfg.Profile
	r := itemResult{attempts: prev.attempts + 1}
	final := func(stage int, drop string) itemResult {
		r.final, r.stage, r.drop = true, stage, drop
		p.cfg.Metrics.drop(src, drop, 1)
		for s := 1; s <= stage && s < len(Stages); s++ {
			p.cfg.Metrics.image(src, Stages[s], 1)
		}
		return r
	}
	// A transient failure is retried on later polls, at most maxAttempts in
	// all; then it is final with its reason.
	transient := func(stage int, drop string) itemResult {
		if r.attempts >= maxAttempts {
			return final(stage, drop)
		}
		return r
	}
	p.mu.Lock()
	poll := p.poll
	p.mu.Unlock()
	deferred := func() itemResult {
		r.attempts = prev.attempts // a deferral is not an attempt
		return r
	}
	// Cheap check first, so a spent budget does not fetch every remaining
	// image of the window on every poll. The reservation itself comes after
	// the fetch: an image already read (the same tile in another mail) costs
	// no call, and a failed fetch costs none either.
	if p.authStopped() {
		return deferred()
	}
	if ok, _ := poll.Available(2); !ok {
		return deferred()
	}
	img, err := p.cfg.Fetcher.FetchImage(ctx, c.Src, prof.ImageHosts)
	if err != nil {
		var fe *FetchError
		if errors.As(err, &fe) {
			if fe.Temporary {
				return transient(0, fe.Reason)
			}
			return final(0, fe.Reason)
		}
		return transient(0, DropFetchFailed)
	}
	sum := sha256.Sum256(img.Bytes)
	reads, cached := p.cachedReads(sum)
	if !cached {
		if ok, _ := poll.Reserve(2); !ok {
			return deferred()
		}
		for i, prompt := range []string{PromptA, PromptB} {
			rd, err := p.cfg.Vision.Read(ctx, img, prompt)
			p.cfg.Budget.record(err == nil)
			if errors.Is(err, ErrVisionAuth) {
				// The key, not the image: refund what was not spent (read A
				// refused: both; read B refused: B), count no attempt, stop
				// this poll's reads.
				poll.Refund(2 - i)
				p.stopForAuth()
				return deferred()
			}
			if err != nil {
				if i == 0 {
					poll.Refund(1) // read B is never made
				}
				if errors.Is(err, ErrBadAnswer) || errors.Is(err, ErrVisionRefused) {
					return final(1, DropVisionFailed)
				}
				return transient(1, DropVisionFailed)
			}
			reads[i] = rd
		}
		p.storeReads(sum, reads)
	}
	prod, why := Agree(reads[0], reads[1])
	if why != "" {
		return final(2, why)
	}
	if !categoryFit(p.cfg.Category, prod.Title) {
		// Before the link is followed: nothing about a laptop is wanted.
		return final(3, DropNotInCategory)
	}
	u, why, temp := p.cfg.Resolver.Resolve(ctx, prof, c)
	switch {
	case why != "" && temp:
		return transient(3, why)
	case why != "":
		return final(3, why)
	}
	if prof.Canonical == CanonicalFollowLink && !slugMatchesTitle(u, prod.Title, prof.SlugHasVintage) {
		return final(3, DropLinkTitleMismatch)
	}
	raw, ok := p.toRaw(prod, u)
	if !ok {
		return final(3, DropDealInvalid)
	}
	r.raw = &raw
	r.final, r.stage, r.drop = true, 4, ""
	for s := 1; s < len(Stages); s++ {
		p.cfg.Metrics.image(src, Stages[s], 1)
	}
	return r
}

// toRaw renders the product as ONE nagus.deal/v1 line and decodes it with the
// same strict validator a human submission meets, then maps it like one.
func (p *Parser) toRaw(prod Product, canonicalURL string) (listing.Raw, bool) {
	prof := p.cfg.Profile
	d := map[string]any{
		"schema":   deal.SchemaID,
		"category": p.cfg.Category,
		"title":    prod.Title,
		"price":    prod.Price,
		"url":      canonicalURL,
		"currency": "USD",
		"seller":   prof.Seller,
	}
	if prod.Brand != "" {
		d["brand"] = prod.Brand
	}
	line, err := json.Marshal(d)
	if err != nil {
		return listing.Raw{}, false
	}
	dl, why := deal.Decode(line)
	if why != deal.ReasonNone {
		return listing.Raw{}, false
	}
	raw := dl.ToRaw(Key(prof.Name, canonicalURL), prof.Principal())
	raw.Aspects[AspectOrigin] = OriginEmailImage
	raw.Aspects[AspectProfile] = prof.Name
	if p.cfg.Vision != nil {
		raw.Aspects[AspectVisionModel] = p.cfg.Vision.Model
	}
	if prod.WasPrice != "" {
		raw.Aspects[AspectCompareAt] = strconv.FormatInt(cents(prod.WasPrice), 10)
	}
	return raw, true
}
