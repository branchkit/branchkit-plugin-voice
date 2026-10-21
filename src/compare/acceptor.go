package compare

import (
	"encoding/json"
	"strings"
)

// startState is the pattern DAG's start, which every command-final arc
// returns to.
const startState = 0

// Acceptor answers "can the live grammar produce exactly these words?" over
// the structured grammar's wire DAG, realized the way the engine realizes it:
// arcs between states; the start state is final; an open state is final and
// loops over its alphabet (none = the full union, so any word). It is
// voice-regress's off-grammar check (the platform's grammar_agreement
// DagAcceptor), so a clip it rejects is skipped here exactly as there.
type Acceptor struct {
	arcs  map[arcKey][]int
	open  map[int]map[string]bool // nil alphabet = any word
	words map[string]bool         // the union
}

type arcKey struct {
	from int
	word string
}

// wireDAG is the slice of the grammar's wire form the acceptor reads.
type wireDAG struct {
	Arcs []struct {
		From int    `json:"from"`
		To   int    `json:"to"`
		Word string `json:"word"`
	} `json:"arcs"`
	OpenStates []struct {
		State    int       `json:"state"`
		Alphabet *[]string `json:"alphabet"`
	} `json:"open_states"`
}

// NewAcceptor builds one from a `pipelines.grammar` full export's
// `vocabulary_update` payload. ok is false when the payload carries no DAG:
// then only the union check applies.
func NewAcceptor(vocabularyUpdate json.RawMessage) (a *Acceptor, ok bool) {
	var p struct {
		Words      []string        `json:"words"`
		NarrowTo   *[]string       `json:"narrow_to"`
		GrammarDAG json.RawMessage `json:"grammar_dag"`
	}
	a = &Acceptor{arcs: map[arcKey][]int{}, open: map[int]map[string]bool{}, words: map[string]bool{}}
	if json.Unmarshal(vocabularyUpdate, &p) != nil {
		return a, false
	}
	// The union the live stage decodes with: narrow_to while an exclusive
	// mode is active, else words.
	union := p.Words
	if p.NarrowTo != nil {
		union = *p.NarrowTo
	}
	for _, w := range union {
		a.words[strings.ToLower(w)] = true
	}
	var dag wireDAG
	if len(p.GrammarDAG) == 0 || string(p.GrammarDAG) == "null" || json.Unmarshal(p.GrammarDAG, &dag) != nil {
		return a, false
	}
	for _, arc := range dag.Arcs {
		k := arcKey{arc.From, strings.ToLower(arc.Word)}
		a.arcs[k] = append(a.arcs[k], arc.To)
	}
	for _, o := range dag.OpenStates {
		if o.Alphabet == nil {
			a.open[o.State] = nil
			continue
		}
		set := map[string]bool{}
		for _, w := range *o.Alphabet {
			set[strings.ToLower(w)] = true
		}
		a.open[o.State] = set
	}
	return a, true
}

// SkipReason is why target cannot be decoded under this grammar, or "": a
// word outside the union, or no path through the DAG.
func (a *Acceptor) SkipReason(target string, haveDAG bool) string {
	words := strings.Fields(Normalize(target))
	var missing []string
	for _, w := range words {
		if !a.words[w] {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		return "words not in the grammar: " + strings.Join(missing, " ")
	}
	if haveDAG && !a.accepts(words) {
		return "no grammar path"
	}
	return ""
}

func (a *Acceptor) accepts(words []string) bool {
	states := map[int]bool{startState: true}
	for _, w := range words {
		next := map[int]bool{}
		for s := range states {
			for _, to := range a.arcs[arcKey{s, w}] {
				next[to] = true
			}
			if alphabet, isOpen := a.open[s]; isOpen && (alphabet == nil || alphabet[w]) {
				next[s] = true
			}
		}
		if len(next) == 0 {
			return false
		}
		states = next
	}
	for s := range states {
		if _, isOpen := a.open[s]; s == startState || isOpen {
			return true
		}
	}
	return false
}
