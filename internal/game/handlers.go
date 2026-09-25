package game

import (
	"log"
	"sort"
	"strings"

	"github.com/fl1X12/phoenix/internal/world"
	"github.com/fl1X12/phoenix/internal/ws"
)

// message dispatches one client envelope. Invalid messages are answered with an error and otherwise ignored.
func (r *Room) message(p *Player, env ws.Envelope) {
	if !r.started {
		p.conn.Send("error", errMsg("game not started"))
		return
	}
	switch env.Type {
	case "interact":
		var d struct {
			ID     string `json:"id"`
			Action string `json:"action"`
			Value  string `json:"value"`
		}
		if !decode(env.Data, &d) || d.ID == "" || d.Action == "" {
			p.conn.Send("error", errMsg("bad interact"))
			return
		}
		r.interact(p, d.ID, d.Action, d.Value)
	case "enter":
		var d struct {
			PortalID string `json:"portalId"`
		}
		if !decode(env.Data, &d) {
			p.conn.Send("error", errMsg("bad enter"))
			return
		}
		r.enter(p, d.PortalID)
	case "room":
		var d struct {
			ID string `json:"id"`
		}
		if decode(env.Data, &d) {
			p.Room = d.ID
		}
	case "join":
		p.conn.Send("error", errMsg("already joined"))
	default:
		p.conn.Send("error", errMsg("unknown type "+env.Type))
	}
}

func (r *Room) interact(p *Player, id, action, value string) {
	side, obj := r.world.SideOf(id)
	if obj == nil || side != p.Side {
		p.conn.Send("error", errMsg("no such object on your side: "+id))
		return
	}
	if !contains(world.ObjectActions[obj.Type], action) {
		p.conn.Send("error", errMsg(obj.Type+" does not accept "+action))
		return
	}

	// Built-in: item pickup.
	if action == "pickup" {
		if r.state[obj.Key] != "home" {
			return
		}
		r.state[obj.Key] = "held"
		r.inv[p.Side][obj.Type] = true
		r.syncInv(p.Side)
		r.fx("actor", p, "pickup", obj.ID)
		return
	}

	// Rules.
	on := id + ":" + action
	fired := 0
	for _, rule := range r.world.Rules {
		if rule.On != on {
			continue
		}
		if ok, why := r.requires(p, rule.Requires, value); !ok {
			log.Printf("room %s: %s by %s blocked: %s", r.Code, on, p.Side, why)
			r.fx("actor", p, why, obj.ID)
			continue
		}
		fired++
		for _, a := range rule.Do {
			r.apply(p, obj, a)
		}
	}
	if fired == 0 {
		log.Printf("room %s: %s by %s: no rule fired", r.Code, on, p.Side)
	}
}

// requires checks a rule's gate; the returned string is the fx effect sent to the actor on failure.
func (r *Room) requires(p *Player, req *world.Requires, value string) (bool, string) {
	if req == nil {
		return true, ""
	}
	if req.Holding != "" && !r.inv[p.Side][req.Holding] {
		return false, "missing_" + req.Holding
	}
	if req.Code != "" {
		want, _ := r.state[req.Code].(string)
		if strings.TrimSpace(value) != want {
			return false, "buzz"
		}
	}
	return true, ""
}

func (r *Room) apply(p *Player, obj *world.Object, a world.Action) {
	switch {
	case a.Toggle != "":
		v, _ := r.state[a.Toggle].(bool)
		r.state[a.Toggle] = !v
	case len(a.Set) > 0:
		for k, v := range a.Set {
			r.state[k] = v
		}
	case a.Fx != "":
		r.fx(a.To, p, a.Fx, obj.ID)
	case a.Consume != "":
		delete(r.inv[p.Side], a.Consume)
		r.syncInv(p.Side)
		if item := r.world.FindByType(p.Side, a.Consume); item != nil {
			r.state[item.Key] = "used"
		}
	case a.Call != "":
		if fn := Calls[a.Call]; fn != nil {
			fn(r, p, obj)
		} else {
			log.Printf("room %s: unknown call %q", r.Code, a.Call)
		}
	}
}

// syncInv mirrors the inventory set into the inv_<side> state key as a sorted list.
func (r *Room) syncInv(side string) {
	var list []string
	for it := range r.inv[side] {
		list = append(list, it)
	}
	sort.Strings(list)
	if list == nil {
		list = []string{}
	}
	r.state["inv_"+side] = list
}

// enter handles a player walking through their exit door.
func (r *Room) enter(p *Player, portalID string) {
	side, obj := r.world.SideOf(portalID)
	if obj == nil || side != p.Side || obj.Type != "exit_door" {
		p.conn.Send("error", errMsg("not your exit door: "+portalID))
		return
	}
	open, _ := r.state[obj.Key].(bool)
	if !open {
		r.fx("actor", p, "locked", obj.ID)
		return
	}
	log.Printf("room %s: side %s crossed the exit, game complete", r.Code, p.Side)
	for _, pl := range r.players {
		if pl.conn != nil {
			pl.conn.Send("game_complete", struct{}{})
		}
	}
	r.done = true
}

// Calls is the escape hatch: rules may name a Go function by string.
var Calls = map[string]func(r *Room, p *Player, obj *world.Object){}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
