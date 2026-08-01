package poller

import (
	"context"
	"sync"

	"github.com/ordureconnoisseur/forager/internal/matcher"
)

// Working out what an adopted download actually is.
//
// Adoption takes ownership of a torrent that was already in the download
// client. Unlike a grab forage made, it arrives with no scene attached: no
// watch, no predicted id, nothing but a release name. All the folder guess had
// to work with was suggest.ConfidentTopFolder, which looks for a local
// performer's full name inside that string and refuses anything ambiguous.
//
// That refusal is right — a confidently wrong folder is worse than Unsorted —
// but it declines a great deal. Of 561 grabs sitting under Unsorted, 357 carry
// no scene id at all, and they are the single biggest thing putting files
// there.
//
// Meanwhile the matcher answers exactly this question, and was measured at
// 0.953 on 1,570 real release titles. It was simply never wired to this path.
// So: when the name guess declines, ask the matcher, and use the scene's own
// cast rather than a substring of the filename.
//
// The conservative principle is unchanged. Only a VERIFIED match names a
// folder. A ranked-but-unverified candidate is exactly the confidently wrong
// answer the name guess exists to avoid, and the verifier already decides that
// question for the whole application.

// matcherFor returns the poller's lazily-built matcher, or nil when StashDB
// is not configured. Construction loads the performer and studio corpora, so
// it is cached the way the API server's is.
func (p *Poller) matcherFor(ctx context.Context) *matcher.Matcher {
	p.matcherMu.Lock()
	defer p.matcherMu.Unlock()
	if p.matcher != nil {
		return p.matcher
	}
	sdb := p.pool.StashDB()
	if sdb == nil || p.db == nil {
		return nil
	}
	m, err := matcher.New(ctx, p.db, sdb)
	if err != nil {
		p.log.Warn("adopt: matcher unavailable", "err", err)
		return nil
	}
	p.matcher = m
	return m
}

// adoptedIdentity is what the matcher could establish about an adopted
// download. Zero value means "no verified answer", which leaves the caller
// exactly where it was.
type adoptedIdentity struct {
	SceneID    string
	Confidence float64
	Performer  string
}

// identifyAdopted matches a release name and returns the scene it verifies
// as, with a performer to file under.
//
// Returns the zero value on every uncertain outcome — no StashDB, no
// candidates, nothing verified — because the caller's fallback (Unsorted) is
// the correct answer when forage does not know.
func (p *Poller) identifyAdopted(ctx context.Context, releaseTitle string) adoptedIdentity {
	if releaseTitle == "" {
		return adoptedIdentity{}
	}
	m := p.matcherFor(ctx)
	if m == nil {
		return adoptedIdentity{}
	}
	cands, err := m.Match(ctx, releaseTitle)
	if err != nil || len(cands) == 0 {
		if err != nil {
			p.log.Warn("adopt: match failed", "release", releaseTitle, "err", err)
		}
		return adoptedIdentity{}
	}
	top := cands[0]
	if !matcher.Verify(cands, top.Scene.ID, top.Scene.Title, releaseTitle).Verified {
		// Ranked but not verified. This is the case the name guess already
		// refuses, for the same reason: a wrong folder is harder to notice,
		// and harder to undo, than an unfiled one.
		return adoptedIdentity{}
	}
	return adoptedIdentity{
		SceneID:    top.Scene.ID,
		Confidence: top.Confidence,
		Performer:  p.castFolder(ctx, top),
	}
}

// castFolder picks the folder name from a matched scene's cast, preferring a
// performer the library already has. Same rule as placementPerformer, which is
// deliberate: a scene should land in the same folder however forage came to
// know what it was.
func (p *Poller) castFolder(ctx context.Context, c matcher.Candidate) string {
	var first string
	for _, perf := range c.Scene.Performers {
		name := perf.Name
		if perf.As != "" {
			name = perf.As
		}
		if name == "" {
			continue
		}
		if first == "" {
			first = name
		}
		if p.localPerformerID(ctx, name) != "" {
			return name
		}
	}
	return first
}

// matcherCache is embedded in Poller. Separate type so the lazily-built
// matcher's mutex is not one more bare field on an already wide struct.
type matcherCache struct {
	matcherMu sync.Mutex
	matcher   *matcher.Matcher
}
