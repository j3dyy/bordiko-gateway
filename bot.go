package main

import (
	"encoding/json"
	"strings"
)

// Bots. A bot is just another move source over the game-host's authoritative
// reducer: the hub notices it's a bot's turn, asks the game-host for that bot's
// redacted view + the legal moves, picks one here, and applies it like any move.
// Because the choice is always taken FROM the enumerated legal moves, a bot can
// never make an illegal play — the ranking below only decides which legal move.
//
// The default policy (any game) is "first legal move" — already rule-legal, since
// the game-host's enumerate enforces every constraint. Jokeri gets a real
// heuristic ("make an effort"): sensible trump, a hand-strength bid, and trick
// play that tries to make its own bid exactly (win cheaply when it still needs
// tricks, duck once it's there).

const botPrefix = "bot:"

func isBot(id string) bool { return strings.HasPrefix(id, botPrefix) }

// moveDesc is one enumerated legal move (type + opaque payload). The payload is
// kept raw so the chosen move is applied to the game-host byte-for-byte.
type moveDesc struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// chooseBotMove returns the move a bot should play from the legal set, given its
// own redacted view. Returns nil only if there are no legal moves.
func chooseBotMove(gameID string, view json.RawMessage, legal []moveDesc) *moveDesc {
	if len(legal) == 0 {
		return nil
	}
	if gameID == "jokeri" {
		if mv := chooseJokeriMove(view, legal); mv != nil {
			return mv
		}
	}
	return &legal[0] // default / fallback: the first legal move is always safe
}

/* ------------------------------- jokeri AI -------------------------------- */

type jkCard struct {
	R string `json:"r"`
	S string `json:"s"`
}

type jkTrickCard struct {
	Player    string `json:"player"`
	Card      jkCard `json:"card"`
	JokerMode string `json:"jokerMode"`
}

// jkView is the subset of Jokeri's player view the bot reasons over.
type jkView struct {
	Phase        string         `json:"phase"`
	Trump        *string        `json:"trump"`
	CalledSuit   *string        `json:"calledSuit"`
	HandSize     int            `json:"handSize"`
	Trick        []jkTrickCard  `json:"trick"`
	Hand         []jkCard       `json:"hand"`
	Bids         map[string]*int `json:"bids"`
	Taken        map[string]int `json:"taken"`
	ToAct        string         `json:"toAct"`
}

// botCand is one legal "play" move under evaluation: does it win the trick as it
// stands, how strong is the card, and is it a Joker (to be spent sparingly).
type botCand struct {
	mv       *moveDesc
	wins     bool
	strength int
	isJoker  bool
}

// jkPlay is a decoded "play" move payload.
type jkPlay struct {
	Card  jkCard `json:"card"`
	Joker *struct {
		Mode string `json:"mode"`
		Suit string `json:"suit"`
	} `json:"joker"`
}

var jkStrength = map[string]int{"6": 0, "7": 1, "8": 2, "9": 3, "10": 4, "J": 5, "Q": 6, "K": 7, "A": 8}

var jkSuits = []string{"S", "C", "D", "H"}

func jkIsJoker(c jkCard) bool { return c.R == "6" && (c.S == "S" || c.S == "C") }

func chooseJokeriMove(view json.RawMessage, legal []moveDesc) *moveDesc {
	var v jkView
	if err := json.Unmarshal(view, &v); err != nil {
		return nil
	}
	switch v.Phase {
	case "trump":
		return chooseTrump(&v, legal)
	case "bid":
		return chooseBid(&v, legal)
	case "play":
		return choosePlay(&v, legal)
	}
	return nil
}

// chooseTrump picks the suit the bot is strongest in (length first, then honour
// weight); with a flat, scattered hand it prefers no-trump. During a 9-card deal
// only the first three cards are visible in the view — the same information a
// human has when calling trump.
func chooseTrump(v *jkView, legal []moveDesc) *moveDesc {
	bySuit := map[string]int{}   // count
	honour := map[string]int{}   // A/K weight
	for _, c := range v.Hand {
		if jkIsJoker(c) {
			continue
		}
		bySuit[c.S]++
		if c.R == "A" {
			honour[c.S] += 2
		} else if c.R == "K" {
			honour[c.S] += 1
		}
	}
	best, bestScore := "", -1
	for _, s := range jkSuits {
		score := bySuit[s]*3 + honour[s]
		if score > bestScore {
			best, bestScore = s, score
		}
	}
	// Scattered (no suit with 2+ cards) and no honour → no-trump is safer.
	pick := best
	if bySuit[best] < 2 && honour[best] == 0 {
		pick = "" // no-trump
	}
	return matchTrump(legal, pick)
}

func matchTrump(legal []moveDesc, suit string) *moveDesc {
	var fallback *moveDesc
	for i := range legal {
		if legal[i].Type != "chooseTrump" {
			continue
		}
		var p struct {
			Trump *string `json:"trump"`
		}
		_ = json.Unmarshal(legal[i].Payload, &p)
		got := ""
		if p.Trump != nil {
			got = *p.Trump
		}
		if got == suit {
			return &legal[i]
		}
		if fallback == nil {
			fallback = &legal[i]
		}
	}
	return fallback
}

// chooseBid estimates how many tricks the hand can win and picks the closest
// legal bid (ties break DOWN — underbidding risks less than a khisht).
func chooseBid(v *jkView, legal []moveDesc) *moveDesc {
	trump := ""
	if v.Trump != nil {
		trump = *v.Trump
	}
	est := 0.0
	trumpLen := 0
	for _, c := range v.Hand {
		if jkIsJoker(c) {
			est += 1.0 // a Joker can always take a trick (played high)
			continue
		}
		if trump != "" && c.S == trump {
			trumpLen++
			switch c.R {
			case "A", "K":
				est += 1.0
			case "Q", "J":
				est += 0.5
			}
			continue
		}
		switch c.R { // side-suit winners
		case "A":
			est += 0.9
		case "K":
			est += 0.45
		}
	}
	// Long trump holdings win extra tricks by ruffing once the suit is drawn.
	if trumpLen > 3 {
		est += float64(trumpLen-3) * 0.6
	}
	target := int(est + 0.5)
	if target < 0 {
		target = 0
	}
	if target > v.HandSize {
		target = v.HandSize
	}
	return closestBid(legal, target)
}

func closestBid(legal []moveDesc, target int) *moveDesc {
	var best *moveDesc
	bestDist := 1 << 30
	for i := range legal {
		if legal[i].Type != "bid" {
			continue
		}
		var p struct {
			Bid int `json:"bid"`
		}
		if json.Unmarshal(legal[i].Payload, &p) != nil {
			continue
		}
		dist := p.Bid - target
		if dist < 0 {
			dist = -dist
		}
		// Strictly-closer wins; on a tie prefer the LOWER bid (bestDist reached
		// first by ascending payloads is already the lower one for equal dist only
		// when the lower comes first — so compare explicitly).
		if best == nil || dist < bestDist {
			best, bestDist = &legal[i], dist
		}
	}
	if best == nil {
		return &legal[0]
	}
	return best
}

// choosePlay is the trick-play heuristic. The bot is self-interested: in Jokeri
// each player is scored on THEIR OWN bid, so it aims to finish the hand having
// taken exactly its bid. While it still needs tricks it wins as cheaply as it
// can; once it has enough it ducks.
func choosePlay(v *jkView, legal []moveDesc) *moveDesc {
	trump := ""
	if v.Trump != nil {
		trump = *v.Trump
	}
	called := ""
	if v.CalledSuit != nil {
		called = *v.CalledSuit
	}
	bid := 0
	if b := v.Bids[v.ToAct]; b != nil {
		bid = *b
	}
	need := bid - v.Taken[v.ToAct] // >0: want more tricks; <=0: duck
	wantWin := need > 0
	leading := len(v.Trick) == 0

	// Decode every legal play with a "wins the trick as it stands" flag and a
	// strength for cheap/expensive ordering.
	cands := make([]botCand, 0, len(legal))
	for i := range legal {
		if legal[i].Type != "play" {
			continue
		}
		var p jkPlay
		if json.Unmarshal(legal[i].Payload, &p) != nil {
			continue
		}
		mode := ""
		callAfter := called
		if p.Joker != nil {
			mode = p.Joker.Mode
			if leading && p.Joker.Suit != "" {
				callAfter = p.Joker.Suit
			}
		} else if leading {
			callAfter = p.Card.S
		}
		trial := append(append([]jkTrickCard{}, v.Trick...), jkTrickCard{Player: v.ToAct, Card: p.Card, JokerMode: mode})
		wins := jkWinner(trial, callAfter, trump) == len(trial)-1
		st := jkStrength[p.Card.R]
		if jkIsJoker(p.Card) {
			if mode == "high" {
				st = 100 // a high Joker is the strongest thing you can commit
			} else {
				st = -1 // a low Joker is the weakest (a deliberate duck)
			}
		}
		cands = append(cands, botCand{mv: &legal[i], wins: wins, strength: st, isJoker: jkIsJoker(p.Card)})
	}
	if len(cands) == 0 {
		return &legal[0]
	}

	if wantWin {
		// Cheapest winning card; prefer not to burn a Joker if a plain card wins.
		var pick *botCand
		for i := range cands {
			c := &cands[i]
			if !c.wins {
				continue
			}
			if pick == nil || betterWinner(c, pick) {
				pick = c
			}
		}
		if pick != nil {
			return pick.mv
		}
		// Can't win this trick → throw the lowest card, but keep Jokers for later.
		return lowestKeepingJokers(cands)
	}

	// Ducking: play the highest card that does NOT win (shed a dangerous high card
	// safely); if every legal card wins, lose the least by playing the lowest.
	var pick *botCand
	for i := range cands {
		c := &cands[i]
		if c.wins || c.isJoker {
			continue
		}
		if pick == nil || c.strength > pick.strength {
			pick = c
		}
	}
	if pick != nil {
		return pick.mv
	}
	return lowestKeepingJokers(cands)
}

// betterWinner: among winning candidates, prefer a cheaper card, and prefer a
// non-Joker over a Joker (save Jokers).
func betterWinner(a, b *botCand) bool {
	if a.isJoker != b.isJoker {
		return !a.isJoker // non-joker preferred
	}
	return a.strength < b.strength
}

func lowestKeepingJokers(cands []botCand) *moveDesc {
	var pick *botCand
	for i := range cands {
		c := &cands[i]
		if c.isJoker {
			continue
		}
		if pick == nil || c.strength < pick.strength {
			pick = c
		}
	}
	if pick != nil {
		return pick.mv
	}
	// Only Jokers left — play one low (the least-committal).
	for i := range cands {
		var p jkPlay
		if json.Unmarshal(cands[i].mv.Payload, &p) == nil && p.Joker != nil && p.Joker.Mode == "low" {
			return cands[i].mv
		}
	}
	return cands[0].mv
}

// jkWinner returns the index of the winning card in a (possibly partial) trick,
// mirroring the game's trickWinner exactly: earliest high-Joker, else highest
// trump, else highest of the called suit, else highest card overall.
func jkWinner(trick []jkTrickCard, called, trump string) int {
	for i := range trick {
		if jkIsJoker(trick[i].Card) && trick[i].JokerMode == "high" {
			return i
		}
	}
	if trump != "" {
		best := -1
		for i := range trick {
			t := trick[i]
			if jkIsJoker(t.Card) || t.Card.S != trump {
				continue
			}
			if best < 0 || jkStrength[t.Card.R] > jkStrength[trick[best].Card.R] {
				best = i
			}
		}
		if best >= 0 {
			return best
		}
	}
	if called != "" {
		best := -1
		for i := range trick {
			t := trick[i]
			if jkIsJoker(t.Card) || t.Card.S != called {
				continue
			}
			if best < 0 || jkStrength[t.Card.R] > jkStrength[trick[best].Card.R] {
				best = i
			}
		}
		if best >= 0 {
			return best
		}
	}
	best := -1
	for i := range trick {
		if jkIsJoker(trick[i].Card) {
			continue
		}
		if best < 0 || jkStrength[trick[i].Card.R] > jkStrength[trick[best].Card.R] {
			best = i
		}
	}
	if best < 0 {
		return 0
	}
	return best
}
