package main

import (
	"encoding/json"
	"testing"
)

func mv(t string, payload any) moveDesc {
	b, _ := json.Marshal(payload)
	return moveDesc{Type: t, Payload: b}
}

func TestIsBot(t *testing.T) {
	if !isBot("bot:1") || isBot("google:123") {
		t.Fatal("isBot")
	}
	if botDisplayName("bot:3") != "Bot 3" {
		t.Fatalf("botDisplayName = %q", botDisplayName("bot:3"))
	}
}

func TestChooseTrumpLongestSuit(t *testing.T) {
	// Two hearts (incl. an Ace) and a lone club → trump Hearts.
	v := &jkView{Phase: "trump", Hand: []jkCard{{"A", "H"}, {"9", "H"}, {"7", "C"}}}
	legal := []moveDesc{
		mv("chooseTrump", map[string]any{"trump": nil}),
		mv("chooseTrump", map[string]any{"trump": "S"}),
		mv("chooseTrump", map[string]any{"trump": "C"}),
		mv("chooseTrump", map[string]any{"trump": "D"}),
		mv("chooseTrump", map[string]any{"trump": "H"}),
	}
	got := chooseTrump(v, legal)
	var p struct {
		Trump *string `json:"trump"`
	}
	_ = json.Unmarshal(got.Payload, &p)
	if p.Trump == nil || *p.Trump != "H" {
		t.Fatalf("expected trump H, got %v", got.Payload)
	}
}

func TestChooseTrumpScatteredNoTrump(t *testing.T) {
	// Three different suits, no honours → no-trump.
	v := &jkView{Phase: "trump", Hand: []jkCard{{"7", "S"}, {"8", "C"}, {"9", "D"}}}
	legal := []moveDesc{
		mv("chooseTrump", map[string]any{"trump": nil}),
		mv("chooseTrump", map[string]any{"trump": "S"}),
		mv("chooseTrump", map[string]any{"trump": "C"}),
		mv("chooseTrump", map[string]any{"trump": "D"}),
		mv("chooseTrump", map[string]any{"trump": "H"}),
	}
	got := chooseTrump(v, legal)
	var p struct {
		Trump *string `json:"trump"`
	}
	_ = json.Unmarshal(got.Payload, &p)
	if p.Trump != nil {
		t.Fatalf("expected no-trump, got %v", got.Payload)
	}
}

func TestChooseBidReasonable(t *testing.T) {
	// A strong hand: a Joker + trump A/K + a side Ace → should bid ~3-4, not pass.
	trump := "H"
	v := &jkView{
		Phase: "bid", HandSize: 9, Trump: &trump,
		Hand: []jkCard{{"6", "S"} /*joker*/, {"A", "H"}, {"K", "H"}, {"A", "S"}, {"7", "D"}, {"8", "D"}, {"9", "C"}, {"7", "C"}, {"8", "S"}},
	}
	legal := make([]moveDesc, 0, 10)
	for b := 0; b <= 9; b++ {
		legal = append(legal, mv("bid", map[string]any{"bid": b}))
	}
	got := chooseBid(v, legal)
	var p struct {
		Bid int `json:"bid"`
	}
	_ = json.Unmarshal(got.Payload, &p)
	if p.Bid < 2 || p.Bid > 5 {
		t.Fatalf("strong hand should bid ~3-4, got %d", p.Bid)
	}
}

func TestChoosePlayWinsCheap(t *testing.T) {
	// Following spades, still need a trick: win with the CHEAPEST card that beats
	// the current leader (9S). K wins, 7 loses → K, but if we also held Q it'd
	// pick Q. Here only K wins so expect K.
	trump := "H"
	called := "S"
	v := &jkView{
		Phase: "play", Trump: &trump, CalledSuit: &called, ToAct: "me",
		Trick: []jkTrickCard{{Player: "p1", Card: jkCard{"9", "S"}}},
		Bids:  map[string]*int{"me": intp(1)}, Taken: map[string]int{"me": 0},
	}
	legal := []moveDesc{
		mv("play", map[string]any{"card": jkCard{"K", "S"}}),
		mv("play", map[string]any{"card": jkCard{"7", "S"}}),
	}
	got := choosePlay(v, legal)
	var p jkPlay
	_ = json.Unmarshal(got.Payload, &p)
	if p.Card.R != "K" {
		t.Fatalf("want-win should play K to beat 9, got %v", p.Card)
	}
}

func TestChoosePlayDucksWhenSatisfied(t *testing.T) {
	// Already made the bid (need<=0): don't win — shed the highest non-winning card.
	trump := "H"
	called := "S"
	v := &jkView{
		Phase: "play", Trump: &trump, CalledSuit: &called, ToAct: "me",
		Trick: []jkTrickCard{{Player: "p1", Card: jkCard{"A", "S"}}}, // A leads → nothing of ours beats it
		Bids:  map[string]*int{"me": intp(0)}, Taken: map[string]int{"me": 0},
	}
	legal := []moveDesc{
		mv("play", map[string]any{"card": jkCard{"K", "S"}}),
		mv("play", map[string]any{"card": jkCard{"7", "S"}}),
	}
	got := choosePlay(v, legal)
	var p jkPlay
	_ = json.Unmarshal(got.Payload, &p)
	// Neither beats the Ace, so both are "safe"; ducking sheds the HIGHER one (K).
	if p.Card.R != "K" {
		t.Fatalf("duck should shed the high K under the winning Ace, got %v", p.Card)
	}
}

func TestChoosePlayTrumpsCheapWhenVoid(t *testing.T) {
	// Void in the led suit, holding two trumps, still needs the trick → ruff with
	// the cheaper trump.
	trump := "H"
	called := "S"
	v := &jkView{
		Phase: "play", Trump: &trump, CalledSuit: &called, ToAct: "me",
		Trick: []jkTrickCard{{Player: "p1", Card: jkCard{"A", "S"}}},
		Bids:  map[string]*int{"me": intp(2)}, Taken: map[string]int{"me": 0},
	}
	legal := []moveDesc{
		mv("play", map[string]any{"card": jkCard{"8", "H"}}),
		mv("play", map[string]any{"card": jkCard{"7", "H"}}),
	}
	got := choosePlay(v, legal)
	var p jkPlay
	_ = json.Unmarshal(got.Payload, &p)
	if p.Card.R != "7" {
		t.Fatalf("should ruff with the cheaper trump 7H, got %v", p.Card)
	}
}

func TestJkWinner(t *testing.T) {
	// Trump beats the led suit.
	w := jkWinner([]jkTrickCard{{Player: "a", Card: jkCard{"A", "S"}}, {Player: "b", Card: jkCard{"7", "H"}}}, "S", "H")
	if w != 1 {
		t.Fatalf("trump should win, got index %d", w)
	}
	// High Joker beats everything.
	w = jkWinner([]jkTrickCard{{Player: "a", Card: jkCard{"A", "H"}}, {Player: "b", Card: jkCard{"6", "S"}, JokerMode: "high"}}, "H", "H")
	if w != 1 {
		t.Fatalf("high joker should win, got index %d", w)
	}
}

func TestChooseBotMoveDefaultFirstLegal(t *testing.T) {
	// An unknown game falls back to the first legal move.
	legal := []moveDesc{mv("x", map[string]any{"a": 1}), mv("y", nil)}
	got := chooseBotMove("some-other-game", json.RawMessage(`{}`), legal)
	if got == nil || got.Type != "x" {
		t.Fatalf("default policy should pick first legal, got %v", got)
	}
}

func intp(n int) *int { return &n }
