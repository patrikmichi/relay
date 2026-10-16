package agentsync

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

type namedNode struct {
	name string
	node *jnode
}

// loadRoot parses the target file, or starts a new document seeded with the
// given top-level keys. ok is false when the existing file is not a JSON
// object, in which case the caller reports it instead of overwriting.
func loadRoot(data []byte, exists bool, seed obj) (root *jnode, indent string, detail string) {
	if !exists || len(strings.TrimSpace(string(data))) == 0 {
		return toNode(seed), "  ", ""
	}
	n, err := parseJSON(data)
	if err != nil {
		if _, jsoncErr := parseJSON(stripJSONC(data)); jsoncErr == nil {
			return nil, "", "not plain JSON: it has comments or trailing commas (JSONC), which zuano cannot edit without losing them; edit it by hand or remove them"
		}
		return nil, "", "not plain JSON (" + err.Error() + "); edit it by hand"
	}
	if n.kind != jObject {
		return nil, "", "top level is not a JSON object"
	}
	n.style = styleOf(data)
	return n, detectIndent(data), ""
}

// container walks objPath below root, creating missing objects when create
// is set. blocked reports a segment that exists but is not an object.
func container(root *jnode, objPath []string, create bool) (cur *jnode, blocked bool) {
	cur = root
	for _, k := range objPath {
		next := cur.get(k)
		if next == nil {
			if !create {
				return nil, false
			}
			next = newObject()
			cur.set(k, next)
		}
		if next.kind != jObject {
			return nil, true
		}
		cur = next
	}
	return cur, false
}

// pruneEmpty removes empty objects along objPath that zuano emptied.
func pruneEmpty(root *jnode, objPath []string) {
	for depth := len(objPath); depth > 0; depth-- {
		c, _ := container(root, objPath[:depth], false)
		if c == nil || len(c.keys) > 0 {
			return
		}
		parent, _ := container(root, objPath[:depth-1], false)
		parent.del(objPath[depth-1])
	}
}

// jsonKeysOp manages named entries inside a JSON object (mcpServers, mcp,
// servers). An entry is zuano's only while the ledger hash matches what is
// on disk.
type jsonKeysOp struct {
	meta
	path    string
	mode    fs.FileMode
	objPath []string
	entries []namedNode
	seed    obj
}

func (o *jsonKeysOp) Run(c *Ctx) ([]Item, error) {
	data, exists, real, err := c.read(o.scope, o.path)
	if items, ok := o.refused(err); ok {
		return items, nil
	}
	if err != nil {
		return nil, err
	}
	led := c.Ledger.entries(real)
	prefix := string(o.kind) + ":"
	if len(o.entries) == 0 && !hasPrefixKey(led, prefix) {
		return nil, nil
	}
	root, indent, bad := loadRoot(data, exists, o.seed)
	// A file with comments or trailing commas is read from a stripped copy and
	// edited in place at the text level, so its comments survive.
	jsonc := false
	if root == nil {
		var ok bool
		if root, indent, ok = loadJSONC(data); !ok {
			return []Item{o.item("*", real, StatusSkipped, bad)}, nil
		}
		jsonc = true
	}

	if _, blocked := container(root, o.objPath, false); blocked {
		return []Item{o.item("*", real, StatusSkipped, strings.Join(o.objPath, ".")+" is not an object")}, nil
	}

	var items []Item
	var muts []jmut
	changed := false
	desired := map[string]bool{}
	for _, e := range o.entries {
		desired[e.name] = true
		id := prefix + e.name
		want := e.node.hash()
		cont, _ := container(root, o.objPath, false)
		var cur *jnode
		if cont != nil {
			cur = cont.get(e.name)
		}
		prev, managed := led[id]
		switch {
		case cur == nil:
			cont, _ = container(root, o.objPath, true)
			cont.set(e.name, e.node)
			muts = append(muts, jmut{key: e.name, val: e.node})
			c.Ledger.set(real, id, want)
			items = append(items, o.item(e.name, real, StatusAdded, ""))
			changed = true
		case cur.hash() == want:
			if managed {
				c.Ledger.set(real, id, want)
			}
			items = append(items, o.item(e.name, real, StatusUnchanged, ownership(managed)))
		case managed && cur.hash() == prev, c.Force:
			cont.set(e.name, e.node)
			muts = append(muts, jmut{key: e.name, val: e.node})
			c.Ledger.set(real, id, want)
			items = append(items, o.item(e.name, real, StatusUpdated, ""))
			changed = true
		case managed:
			items = append(items, o.item(e.name, real, StatusConflict, "edited since zuano wrote it; --force overwrites"))
		default:
			items = append(items, o.item(e.name, real, StatusConflict, "already defined and not managed by zuano; --force overwrites"))
		}
	}

	stale := make([]string, 0)
	for id := range led {
		if strings.HasPrefix(id, prefix) && !desired[strings.TrimPrefix(id, prefix)] {
			stale = append(stale, id)
		}
	}
	sort.Strings(stale)
	for _, id := range stale {
		name := strings.TrimPrefix(id, prefix)
		cont, _ := container(root, o.objPath, false)
		var cur *jnode
		if cont != nil {
			cur = cont.get(name)
		}
		switch {
		case cur == nil:
			c.Ledger.drop(real, id)
		case cur.hash() == led[id] || c.Force:
			cont.del(name)
			muts = append(muts, jmut{key: name, del: true})
			c.Ledger.drop(real, id)
			items = append(items, o.item(name, real, StatusRemoved, ""))
			changed = true
		default:
			c.Ledger.drop(real, id)
			items = append(items, o.item(name, real, StatusSkipped, "edited since zuano wrote it; left in place and no longer managed"))
		}
	}

	switch {
	case changed && jsonc:
		// The user's file stays, even when zuano emptied the object it keeps.
		out, err := editJSONC(data, o.objPath, muts, indent)
		if err != nil {
			return nil, fmt.Errorf("edit %s without losing its comments: %w", real, err)
		}
		if err := c.write(real, o.mode, out); err != nil {
			return nil, err
		}
	case changed:
		pruneEmpty(root, o.objPath)
		if err := writeRoot(c, real, o.mode, root, indent, o.seed); err != nil {
			return nil, err
		}
	}
	if o.kind == KindMCP {
		for i := range items {
			if items[i].Name != "*" {
				items[i].Server = items[i].Name
			}
		}
	}
	return items, nil
}

func ownership(managed bool) string {
	if managed {
		return ""
	}
	return "identical entry already present, not managed by zuano"
}

func hasPrefixKey(m map[string]string, prefix string) bool {
	for k := range m {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// eventItems are the desired array items for one hook event.
type eventItems struct {
	event string
	items []namedNode
}

// jsonItemsOp manages array items under per-event keys (hooks). Items are
// identified by content hash; an item the user edited stops being managed
// and zuano will not add a duplicate when an equivalent hook is present.
type jsonItemsOp struct {
	meta
	path    string
	mode    fs.FileMode
	objPath []string
	events  []eventItems
	// signature returns the matcher and commands of an item so a hook the
	// user already wrote by hand is not duplicated.
	signature func(*jnode) (matcher string, commands []string)
	seed      obj
}

func (o *jsonItemsOp) Run(c *Ctx) ([]Item, error) {
	data, exists, real, err := c.read(o.scope, o.path)
	if items, ok := o.refused(err); ok {
		return items, nil
	}
	if err != nil {
		return nil, err
	}
	led := c.Ledger.entries(real)
	if len(o.events) == 0 && !hasPrefixKey(led, "hook:") && !hasPrefixKey(led, hookSourcePrefix) {
		return nil, nil
	}
	root, indent, bad := loadRoot(data, exists, o.seed)
	if root == nil {
		return []Item{o.item("*", real, StatusSkipped, bad)}, nil
	}

	desiredByEvent := map[string][]namedNode{}
	for _, e := range o.events {
		desiredByEvent[e.event] = e.items
	}
	events := map[string]bool{}
	for ev := range desiredByEvent {
		events[ev] = true
	}
	for id := range led {
		// A hand-edited ledger can hold a malformed id; it names no event.
		for _, prefix := range []string{"hook:", hookSourcePrefix} {
			if rest, ok := strings.CutPrefix(id, prefix); ok {
				if i := strings.Index(rest, ":"); i > 0 {
					events[rest[:i]] = true
				}
			}
		}
	}
	names := make([]string, 0, len(events))
	for ev := range events {
		names = append(names, ev)
	}
	sort.Strings(names)

	if _, blocked := container(root, o.objPath, false); blocked {
		return []Item{o.item("*", real, StatusSkipped, strings.Join(o.objPath, ".")+" is not an object")}, nil
	}

	var items []Item
	changed := false
	for _, ev := range names {
		cont, _ := container(root, o.objPath, false)
		var arr *jnode
		if cont != nil {
			arr = cont.get(ev)
		}
		if arr != nil && arr.kind != jArray {
			items = append(items, o.item(ev, real, StatusSkipped, "hooks."+ev+" is not an array"))
			continue
		}
		var existing []*jnode
		if arr != nil {
			existing = append(existing, arr.arr...)
		}
		wantHashes, wantSources := map[string]bool{}, map[string]bool{}
		for _, d := range desiredByEvent[ev] {
			wantHashes[d.node.hash()] = true
			wantSources[o.sourceKey(d.node)] = true
		}

		kept := make([]*jnode, 0, len(existing))
		present := map[string]bool{}
		for _, it := range existing {
			h := it.hash()
			id := "hook:" + ev + ":" + h
			src := hookSourcePrefix + ev + ":" + o.sourceKey(it)
			if _, managed := led[id]; managed && !wantHashes[h] {
				c.Ledger.drop(real, id)
				c.Ledger.drop(real, src)
				items = append(items, o.item(ev, real, StatusRemoved, "stale hook"))
				changed = true
				continue
			}
			// An item whose bytes no longer match what zuano wrote but whose
			// matcher and command still do is zuano's hook, edited by hand.
			if _, wasWritten := led[src]; wasWritten && !managed(led, id) && !wantSources[o.sourceKey(it)] {
				if !c.Force {
					items = append(items, o.item(ev, real, StatusSkipped, "a hook zuano wrote was edited since; left in place, --force removes it"))
				} else {
					c.Ledger.drop(real, src)
					items = append(items, o.item(ev, real, StatusRemoved, "edited hook removed (--force)"))
					changed = true
					continue
				}
			}
			present[h] = true
			kept = append(kept, it)
		}
		// Forget what zuano wrote once no item carries it any more.
		keptSources := map[string]bool{}
		for _, it := range kept {
			keptSources[o.sourceKey(it)] = true
		}
		for id := range led {
			if rest, ok := strings.CutPrefix(id, hookSourcePrefix+ev+":"); ok && !keptSources[rest] && !wantSources[rest] {
				c.Ledger.drop(real, id)
			}
		}
		for id := range led {
			if rest, ok := strings.CutPrefix(id, "hook:"+ev+":"); ok && !present[rest] {
				c.Ledger.drop(real, id)
			}
		}

		for _, d := range desiredByEvent[ev] {
			h := d.node.hash()
			id := "hook:" + ev + ":" + h
			if present[h] {
				if managed(led, id) {
					c.Ledger.set(real, id, h)
					c.Ledger.set(real, hookSourcePrefix+ev+":"+o.sourceKey(d.node), h)
				}
				items = append(items, o.item(d.name, real, StatusUnchanged, ev))
				continue
			}
			if o.userHas(kept, d.node) {
				items = append(items, o.item(d.name, real, StatusSkipped, ev+": an equivalent hook already exists"))
				continue
			}
			kept = append(kept, d.node)
			present[h] = true
			c.Ledger.set(real, id, h)
			c.Ledger.set(real, hookSourcePrefix+ev+":"+o.sourceKey(d.node), h)
			items = append(items, o.item(d.name, real, StatusAdded, ev))
			changed = true
		}

		if len(kept) == 0 {
			if arr != nil {
				cont.del(ev)
			}
			continue
		}
		if arr == nil {
			arr = &jnode{kind: jArray}
			cont, _ = container(root, o.objPath, true)
			cont.set(ev, arr)
		}
		arr.arr = kept
	}

	if changed {
		pruneEmpty(root, o.objPath)
		if err := writeRoot(c, real, o.mode, root, indent, o.seed); err != nil {
			return nil, err
		}
	}
	return items, nil
}

// hookSourcePrefix keys the ledger by what a hook is (its event, matcher and
// commands) rather than by its bytes, so a hook zuano wrote can still be
// recognised after the person edits it.
const hookSourcePrefix = "hooksrc:"

func managed(led map[string]string, id string) bool {
	_, ok := led[id]
	return ok
}

// sourceKey identifies a hook item by its matcher and commands.
func (o *jsonItemsOp) sourceKey(n *jnode) string {
	m, cmds := o.signature(n)
	return contentHash([]byte(m + "\x00" + strings.Join(cmds, "\x00")))
}

func (o *jsonItemsOp) userHas(existing []*jnode, want *jnode) bool {
	wm, wc := o.signature(want)
	for _, it := range existing {
		m, cmds := o.signature(it)
		if m != wm {
			continue
		}
		for _, a := range cmds {
			for _, b := range wc {
				if a == b {
					return true
				}
			}
		}
	}
	return false
}

// emptyRoot reports a document holding nothing but the keys zuano seeds into
// new files, which is not worth keeping once zuano removed its entries.
func emptyRoot(root *jnode, seed obj) bool {
	seeded := map[string]string{}
	for _, e := range seed {
		seeded[e.k] = toNode(e.v).hash()
	}
	for _, k := range root.keys {
		if h, ok := seeded[k]; !ok || root.vals[k].hash() != h {
			return false
		}
	}
	return true
}

func writeRoot(c *Ctx, real string, mode fs.FileMode, root *jnode, indent string, seed obj) error {
	if emptyRoot(root, seed) {
		return c.remove(real)
	}
	return c.write(real, mode, root.style.apply(root.marshal(indent)))
}
