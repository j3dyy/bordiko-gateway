package main

import (
	"encoding/json"
	"strconv"
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
	// The game-host view is {G: {...game state...}, phase, currentPlayer, ...}. A
	// bot reasons over the GAME state, so unwrap G before handing it to the
	// game-specific chooser (whose view structs read the game's own fields).
	g := view
	var wrap struct {
		G json.RawMessage `json:"G"`
	}
	if json.Unmarshal(view, &wrap) == nil && len(wrap.G) > 0 {
		g = wrap.G
	}
	if gameID == "jokeri" {
		if mv := chooseJokeriMove(g, legal); mv != nil {
			return mv
		}
	}
	if gameID == "avalon" {
		if mv := chooseAvalonMove(g, legal); mv != nil {
			return mv
		}
	}
	if gameID == "kartuli-express" {
		if mv := chooseKartuliMove(legal); mv != nil {
			return mv
		}
	}
	return &legal[0] // default / fallback: the first legal move is always safe
}

/* --------------------------- kartuli-express AI --------------------------- */

// A bot that actually builds a network: resolve any passport keep, otherwise
// claim the longest route it can afford (most points, and it drains tokens so the
// game progresses toward its end), and only draw when nothing is claimable. The
// default "first legal move" would just draw from the deck forever and never
// claim, so the bot would appear to do nothing.
func chooseKartuliMove(legal []moveDesc) *moveDesc {
	// 1) Passport keep decision (setup or Action C): take the first valid keep.
	for i := range legal {
		if legal[i].Type == "keepPassports" {
			return &legal[i]
		}
	}
	// 2) Claim the longest affordable route (cards paid == route length).
	best := 0
	var bestMv *moveDesc
	for i := range legal {
		if legal[i].Type != "claim" {
			continue
		}
		var p struct {
			Cards []json.RawMessage `json:"cards"`
		}
		_ = json.Unmarshal(legal[i].Payload, &p)
		if len(p.Cards) > best {
			best = len(p.Cards)
			bestMv = &legal[i]
		}
	}
	if bestMv != nil {
		return bestMv
	}
	// 3) Otherwise draw a card (build toward routes); avoid drawing new passports.
	for i := range legal {
		if legal[i].Type == "draw" {
			return &legal[i]
		}
	}
	return &legal[0]
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

/* ------------------------------- avalon AI -------------------------------- */

// A role-aware Avalon bot. It reasons over the SAME redacted board a human sees,
// so it only knows what its role is allowed to know: its own faction always, and
// (for evil, and for Merlin) the seats it can see the allegiance of. Good bots
// approve teams with no visible evil and always play Success; evil bots get on
// teams, approve teams carrying an evil, fail quests, and — as the Assassin —
// guess a seat they DON'T know to be evil (their partners can't be Merlin).

const avTeamEvil = "#D83A34" // TEAM_EVIL in games/avalon: a seat the viewer sees as evil

type avSeat struct {
	ID     string   `json:"id"`
	Role   string   `json:"role"`
	Color  string   `json:"color"`
	Badges []string `json:"badges"`
}

type avView struct {
	Board struct {
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
		Seats  []avSeat `json:"seats"`
		Tracks []struct {
			Steps []struct {
				Label string `json:"label"`
				State string `json:"state"`
			} `json:"steps"`
		} `json:"tracks"`
	} `json:"board"`
}

func avContains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func chooseAvalonMove(view json.RawMessage, legal []moveDesc) *moveDesc {
	var v avView
	if err := json.Unmarshal(view, &v); err != nil {
		return nil
	}
	var me *avSeat
	for i := range v.Board.Seats {
		if avContains(v.Board.Seats[i].Badges, "You") {
			me = &v.Board.Seats[i]
		}
	}
	iAmEvil := me != nil && me.Color == avTeamEvil

	switch v.Board.Status.Phase {
	case "night":
		return &legal[0] // only "ready" is legal — acknowledge and move on
	case "team":
		return avPropose(&v, me, iAmEvil, legal)
	case "vote":
		return avVote(&v, iAmEvil, legal)
	case "quest":
		return avQuestMove(legal, !iAmEvil) // good must succeed; evil fails to sink it
	case "assassin":
		return avAssassinate(&v, legal)
	}
	return nil
}

// avQuestSize reads the current quest's required team size off the quest track.
func avQuestSize(v *avView) int {
	for _, t := range v.Board.Tracks {
		for _, s := range t.Steps {
			if s.State == "current" {
				if n, err := strconv.Atoi(s.Label); err == nil {
					return n
				}
			}
		}
	}
	return 0
}

// avPropose builds a team of the required size: always include self, then good
// prefers seats it does NOT see as evil, while evil seeds one visible partner (a
// guaranteed fail) before filling the rest.
func avPropose(v *avView, me *avSeat, iAmEvil bool, legal []moveDesc) *moveDesc {
	size := avQuestSize(v)
	if size <= 0 || me == nil {
		return &legal[0]
	}
	team := []string{me.ID}
	var pref, rest []string
	for _, s := range v.Board.Seats {
		if s.ID == me.ID {
			continue
		}
		evilSeat := s.Color == avTeamEvil
		if (iAmEvil && evilSeat) || (!iAmEvil && !evilSeat) {
			pref = append(pref, s.ID)
		} else {
			rest = append(rest, s.ID)
		}
	}
	for _, id := range append(pref, rest...) {
		if len(team) >= size {
			break
		}
		team = append(team, id)
	}
	if len(team) != size {
		return &legal[0]
	}
	payload, _ := json.Marshal(map[string]any{"players": team})
	return &moveDesc{Type: "proposeTeam", Payload: payload}
}

// avVote: good approves a team with no visible evil; evil approves a team that
// carries an evil it can see (itself or a partner), else rejects.
func avVote(v *avView, iAmEvil bool, legal []moveDesc) *moveDesc {
	teamHasEvil := false
	for _, s := range v.Board.Seats {
		if avContains(s.Badges, "On team") || avContains(s.Badges, "On quest") {
			if s.Color == avTeamEvil {
				teamHasEvil = true
			}
		}
	}
	approve := teamHasEvil
	if !iAmEvil {
		approve = !teamHasEvil
	}
	for i := range legal {
		var p struct {
			Approve bool `json:"approve"`
		}
		if json.Unmarshal(legal[i].Payload, &p) == nil && p.Approve == approve {
			return &legal[i]
		}
	}
	return &legal[0]
}

func avQuestMove(legal []moveDesc, success bool) *moveDesc {
	for i := range legal {
		var p struct {
			Success bool `json:"success"`
		}
		if json.Unmarshal(legal[i].Payload, &p) == nil && p.Success == success {
			return &legal[i]
		}
	}
	return &legal[0]
}

// avAssassinate guesses Merlin among the seats the Assassin does NOT already know
// to be evil (its partners can't be Merlin).
func avAssassinate(v *avView, legal []moveDesc) *moveDesc {
	evilIDs := map[string]bool{}
	for _, s := range v.Board.Seats {
		if s.Color == avTeamEvil {
			evilIDs[s.ID] = true
		}
	}
	for i := range legal {
		var p struct {
			Target string `json:"target"`
		}
		if json.Unmarshal(legal[i].Payload, &p) == nil && !evilIDs[p.Target] {
			return &legal[i]
		}
	}
	return &legal[0]
}
