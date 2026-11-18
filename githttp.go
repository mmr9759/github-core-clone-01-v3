// HTTP handlers for the git smart-HTTP endpoints.
package main

import (
	"bytes"
	"sort"
	"strings"
)

const uploadPackCaps = "multi_ack thin-pack side-band-64k ofs-delta shallow symref=HEAD:refs/heads/main agent=forge/1.0"
const receivePackCaps = "report-status side-band-64k delete-refs ofs-delta agent=forge/1.0"

func (s *server) dispatchGit(req *request, owner, repoName, suffix string) response {
	switch suffix {
	case "info/refs":
		if req.method != "GET" {
			return errJSON(405, "method not allowed")
		}
		return s.gitInfoRefs(req, owner, repoName)
	case "git-upload-pack":
		if req.method != "POST" {
			return errJSON(405, "method not allowed")
		}
		return s.gitUploadPack(req, owner, repoName)
	case "git-receive-pack":
		if req.method != "POST" {
			return errJSON(405, "method not allowed")
		}
		return s.gitReceivePack(req, owner, repoName)
	}
	return errJSON(404, "Not Found")
}

func (s *server) lookupRepo(owner, name string) *repo {
	s.st.mu.RLock()
	defer s.st.mu.RUnlock()
	return s.st.getRepo(owner, name)
}

func (s *server) gitInfoRefs(req *request, owner, repoName string) response {
	r := s.lookupRepo(owner, repoName)
	if r == nil {
		return errJSON(404, "Not Found")
	}
	service := ""
	if v, ok := req.query["service"]; ok && len(v) > 0 {
		service = v[0]
	}
	if service != "git-upload-pack" && service != "git-receive-pack" {
		return errJSON(400, "missing service")
	}
	if service == "git-upload-pack" && req.headers["git-protocol"] == "version=2" {
		return response{status: 200, contentType: "application/x-git-upload-pack-advertisement", body: advertisementV2()}
	}
	return response{status: 200, contentType: "application/x-" + service + "-advertisement", body: s.advertisementV0(r, service)}
}

func advertisementV2() []byte {
	var b bytes.Buffer
	b.Write(pktStr("version 2\n"))
	b.Write(pktStr("ls-refs=unborn\n"))
	b.Write(pktStr("fetch=shallow\n"))
	b.Write(pktStr("agent=forge/1.0\n"))
	b.Write(pktStr("object-format=sha1\n"))
	b.Write(pktFlush)
	return b.Bytes()
}

func (s *server) advertisementV0(r *repo, service string) []byte {
	s.st.mu.RLock()
	defer s.st.mu.RUnlock()
	caps := uploadPackCaps
	if service == "git-receive-pack" {
		caps = receivePackCaps
	}
	var b bytes.Buffer
	b.Write(pktStr("# service=" + service + "\n"))
	b.Write(pktFlush)
	refs := sortedRefNames(r)
	if len(refs) == 0 {
		b.Write(pktStr(zeroOID + " capabilities^{}\x00" + caps + "\n"))
	} else {
		tip := r.refs["refs/heads/"+r.defaultBranch]
		if tip == "" {
			tip = r.refs[refs[0]]
		}
		b.Write(pktStr(tip + " HEAD\x00" + caps + "\n"))
		for _, name := range refs {
			oid := r.refs[name]
			b.Write(pktStr(oid + " " + name + "\n"))
			if o := r.objects[oid]; o != nil && o.typ == "tag" {
				if target := tagTarget(o.data); target != "" {
					b.Write(pktStr(target + " " + name + "^{}\n"))
				}
			}
		}
	}
	b.Write(pktFlush)
	return b.Bytes()
}

func sortedRefNames(r *repo) []string {
	names := make([]string, 0, len(r.refs))
	for name := range r.refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func tagTarget(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "object ") {
			return strings.TrimSpace(line[7:])
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// upload-pack
// ---------------------------------------------------------------------------

func (s *server) gitUploadPack(req *request, owner, repoName string) response {
	r := s.lookupRepo(owner, repoName)
	if r == nil {
		return errJSON(404, "Not Found")
	}
	if req.headers["git-protocol"] == "version=2" {
		return s.fetchV2(req, r)
	}
	return s.fetchV0(req, r)
}

// parseV2FetchArgs reads a v2 fetch request body into a fetchRequest.
func parseV2FetchArgs(body []byte) (fetchRequest, bool) {
	var fr fetchRequest
	fr.depth = -1
	pr := &pktReader{data: body}
	inArgs := false
	for {
		kind, payload := pr.next()
		switch kind {
		case "eof", "bad":
			return fr, false
		case "flush":
			return fr, true
		case "delim":
			inArgs = true
		case "data":
			line := strings.TrimSuffix(string(payload), "\n")
			if !inArgs {
				continue
			}
			switch {
			case strings.HasPrefix(line, "want "):
				fr.wants = append(fr.wants, strings.Fields(line[5:])[0])
			case strings.HasPrefix(line, "have "):
				fr.haves = append(fr.haves, strings.TrimSpace(line[5:]))
			case strings.HasPrefix(line, "shallow "):
				fr.shallow = append(fr.shallow, strings.TrimSpace(line[8:]))
			case strings.HasPrefix(line, "deepen "):
				fr.depth = int(atoi64(strings.TrimSpace(line[7:])))
			case strings.HasPrefix(line, "deepen-since "):
				fr.deepenSince = atoi64(strings.TrimSpace(line[13:]))
			case strings.HasPrefix(line, "deepen-not "):
				fr.deepenNot = append(fr.deepenNot, strings.TrimSpace(line[11:]))
			}
		}
	}
}

// parseV0FetchArgs reads a v0 fetch request: wants section, flush, haves section.
func parseV0FetchArgs(body []byte) (fetchRequest, bool) {
	var fr fetchRequest
	fr.depth = -1
	pr := &pktReader{data: body}
	section := 0
	for {
		kind, payload := pr.next()
		switch kind {
		case "eof", "bad":
			return fr, false
		case "flush":
			section++
			if section >= 2 {
				return fr, true
			}
		case "data":
			line := strings.TrimSuffix(string(payload), "\n")
			switch {
			case strings.HasPrefix(line, "want "):
				fields := strings.Fields(line[5:])
				if len(fields) > 0 {
					fr.wants = append(fr.wants, fields[0])
				}
			case strings.HasPrefix(line, "have "):
				fr.haves = append(fr.haves, strings.TrimSpace(line[5:]))
			case strings.HasPrefix(line, "shallow "):
				fr.shallow = append(fr.shallow, strings.TrimSpace(line[8:]))
			case strings.HasPrefix(line, "deepen "):
				fr.depth = int(atoi64(strings.TrimSpace(line[7:])))
			case strings.HasPrefix(line, "deepen-since "):
				fr.deepenSince = atoi64(strings.TrimSpace(line[13:]))
			case strings.HasPrefix(line, "deepen-not "):
				fr.deepenNot = append(fr.deepenNot, strings.TrimSpace(line[11:]))
			}
		}
	}
}

// parseLsRefsArgs reads an ls-refs request, returning peel/symrefs/unborn and prefixes.
func parseLsRefsArgs(body []byte) (peel, symrefs, unborn bool, prefixes []string, ok bool) {
	pr := &pktReader{data: body}
	inArgs := false
	for {
		kind, payload := pr.next()
		switch kind {
		case "eof", "bad":
			return false, false, false, nil, false
		case "flush":
			return peel, symrefs, unborn, prefixes, true
		case "delim":
			inArgs = true
		case "data":
			if !inArgs {
				continue
			}
			line := strings.TrimSuffix(string(payload), "\n")
			switch {
			case line == "peel":
				peel = true
			case line == "symrefs":
				symrefs = true
			case line == "unborn":
				unborn = true
			case strings.HasPrefix(line, "ref-prefix "):
				prefixes = append(prefixes, strings.TrimSpace(line[11:]))
			}
		}
	}
}

func (s *server) fetchV2(req *request, r *repo) response {
	// ls-refs is also a v2 POST to git-upload-pack; it starts with command=ls-refs.
	if bytes.HasPrefix(req.body, []byte("0014command=ls-refs")) || v2Command(req.body) == "ls-refs" {
		return s.lsRefs(req, r)
	}
	fr, ok := parseV2FetchArgs(req.body)
	if !ok {
		return errJSON(400, "bad request")
	}
	s.st.mu.RLock()
	plan := r.planFetch(fr)
	pack := s.cachedPack(r, fr, plan)
	s.st.mu.RUnlock()
	var b bytes.Buffer
	b.Write(pktStr("acknowledgments\n"))
	if plan.ack != "" {
		b.Write(pktStr("ACK " + plan.ack + "\n"))
		b.Write(pktStr("ready\n"))
	} else {
		b.Write(pktStr("NAK\n"))
	}
	b.Write(pktFlush)
	if fr.depth > 0 || fr.deepenSince > 0 || len(plan.unshallow) > 0 {
		b.Write(pktStr("shallow-info\n"))
		for _, oid := range plan.shallowOut {
			b.Write(pktStr("shallow " + oid + "\n"))
		}
		for _, oid := range plan.unshallow {
			b.Write(pktStr("unshallow " + oid + "\n"))
		}
		b.Write(pktFlush)
	}
	b.Write(pktStr("packfile\n"))
	writeSideband(&b, pack)
	b.Write(pktFlush)
	return response{status: 200, contentType: "application/x-git-upload-pack-result", body: b.Bytes()}
}

// v2Command peeks at the first pkt-line to find a command= line.
func v2Command(body []byte) string {
	pr := &pktReader{data: body}
	for i := 0; i < 8; i++ {
		kind, payload := pr.next()
		if kind != "data" {
			return ""
		}
		line := strings.TrimSuffix(string(payload), "\n")
		if strings.HasPrefix(line, "command=") {
			return strings.TrimSpace(line[8:])
		}
	}
	return ""
}

func (s *server) lsRefs(req *request, r *repo) response {
	peel, symrefs, unborn, prefixes, ok := parseLsRefsArgs(req.body)
	if !ok {
		return errJSON(400, "bad request")
	}
	s.st.mu.RLock()
	defer s.st.mu.RUnlock()
	matches := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var b bytes.Buffer
	names := sortedRefNames(r)
	if len(names) == 0 {
		if unborn && matches("HEAD") {
			b.Write(pktStr("unborn HEAD symref-target:refs/heads/" + r.defaultBranch + "\n"))
		}
		b.Write(pktFlush)
		return response{status: 200, contentType: "application/x-git-upload-pack-result", body: b.Bytes()}
	}
	tip := r.refs["refs/heads/"+r.defaultBranch]
	if tip != "" && matches("HEAD") {
		line := tip + " HEAD"
		if symrefs {
			line += " symref-target:refs/heads/" + r.defaultBranch
		}
		b.Write(pktStr(line + "\n"))
	}
	for _, name := range names {
		if !matches(name) {
			continue
		}
		oid := r.refs[name]
		line := oid + " " + name
		if peel {
			if o := r.objects[oid]; o != nil && o.typ == "tag" {
				if target := tagTarget(o.data); target != "" {
					line += " peeled:" + target
				}
			}
		}
		b.Write(pktStr(line + "\n"))
	}
	b.Write(pktFlush)
	return response{status: 200, contentType: "application/x-git-upload-pack-result", body: b.Bytes()}
}

func (s *server) fetchV0(req *request, r *repo) response {
	fr, ok := parseV0FetchArgs(req.body)
	if !ok {
		return errJSON(400, "bad request")
	}
	s.st.mu.RLock()
	plan := r.planFetch(fr)
	pack := s.cachedPack(r, fr, plan)
	s.st.mu.RUnlock()
	var b bytes.Buffer
	if plan.ack != "" {
		b.Write(pktStr("ACK " + plan.ack + "\n"))
	} else {
		b.Write(pktStr("NAK\n"))
	}
	for _, oid := range plan.shallowOut {
		b.Write(pktStr("shallow " + oid + "\n"))
	}
	for _, oid := range plan.unshallow {
		b.Write(pktStr("unshallow " + oid + "\n"))
	}
	writeSideband(&b, pack)
	b.Write(pktFlush)
	return response{status: 200, contentType: "application/x-git-upload-pack-result", body: b.Bytes()}
}

// cachedPack returns pack bytes for a fetch plan, using the repo pack cache.
func (s *server) cachedPack(r *repo, fr fetchRequest, plan fetchPlan) []byte {
	key := packCacheKey(r, fr)
	if pack, ok := r.packCache[key]; ok {
		return pack
	}
	pack := writePackFromRepo(r, plan.packOIDs)
	// Insert under the write lock (caller holds RLock; upgrade briefly).
	s.st.mu.RUnlock()
	s.st.mu.Lock()
	if len(r.packCache) > 128 {
		r.packCache = map[string][]byte{}
	}
	r.packCache[key] = pack
	s.st.mu.Unlock()
	s.st.mu.RLock()
	return pack
}

// writeSideband writes pack bytes as band-1 sideband packets (max 65515 bytes each).
func writeSideband(b *bytes.Buffer, pack []byte) {
	const maxChunk = 65515
	if len(pack) == 0 {
		pack = writePackFromRepo(nil, nil)
	}
	for len(pack) > 0 {
		n := len(pack)
		if n > maxChunk {
			n = maxChunk
		}
		payload := make([]byte, 0, n+1)
		payload = append(payload, 1)
		payload = append(payload, pack[:n]...)
		b.Write(pktLine(payload))
		pack = pack[n:]
	}
}

// ---------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------

type pushCommand struct {
	old string
	new string
	ref string
}

func (s *server) gitReceivePack(req *request, owner, repoName string) response {
	r := s.lookupRepo(owner, repoName)
	if r == nil {
		return errJSON(404, "Not Found")
	}
	// Parse command pkt-lines until flush; the rest of the body is the pack.
	var cmds []pushCommand
	packStart := -1
	pr := &pktReader{data: req.body}
	for {
		kind, payload := pr.next()
		switch kind {
		case "eof", "bad":
			return errJSON(400, "bad request")
		case "flush":
			packStart = pr.pos
			goto parsed
		case "data":
			line := strings.TrimSuffix(string(payload), "\n")
			if idx := strings.IndexByte(line, 0); idx >= 0 {
				line = line[:idx]
			}
			fields := strings.Fields(line)
			if len(fields) != 3 || !isOID(fields[0]) || !isOID(fields[1]) || !strings.HasPrefix(fields[2], "refs/") {
				return errJSON(400, "bad command")
			}
			cmds = append(cmds, pushCommand{old: fields[0], new: fields[1], ref: fields[2]})
		}
	}
parsed:
	packBytes := req.body[packStart:]
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	unpackOK := true
	if len(packBytes) > 0 {
		if !verifyPackChecksum(packBytes) {
			unpackOK = false
		} else if objs, _, err := readPack(r, packBytes); err != nil {
			unpackOK = false
		} else {
			for oid, o := range objs {
				_ = oid
				r.storeObject(o.typ, o.data)
			}
		}
	}
	var inner bytes.Buffer
	if unpackOK {
		inner.Write(pktStr("unpack ok\n"))
	} else {
		inner.Write(pktStr("unpack error\n"))
	}
	if unpackOK {
		for _, cmd := range cmds {
			ok, reason := s.applyPushCommand(r, cmd)
			if ok {
				inner.Write(pktStr("ok " + cmd.ref + "\n"))
			} else {
				inner.Write(pktStr("ng " + cmd.ref + " " + reason + "\n"))
			}
		}
	}
	inner.Write(pktFlush)
	var outer bytes.Buffer
	payload := make([]byte, 0, inner.Len()+1)
	payload = append(payload, 1)
	payload = append(payload, inner.Bytes()...)
	outer.Write(pktLine(payload))
	outer.Write(pktFlush)
	return response{status: 200, contentType: "application/x-git-receive-pack-result", body: outer.Bytes()}
}

// applyPushCommand validates and applies one ref update. Returns ok + ng reason.
func (s *server) applyPushCommand(r *repo, cmd pushCommand) (bool, string) {
	cur, exists := r.refs[cmd.ref]
	if cmd.new == zeroOID {
		// Delete.
		if !exists {
			return false, "remote ref does not exist"
		}
		delete(r.refs, cmd.ref)
		r.version++
		return true, ""
	}
	newObj := r.objects[cmd.new]
	if newObj == nil || (newObj.typ != "commit" && newObj.typ != "tag") {
		return false, "object not found"
	}
	newCommit := cmd.new
	if newObj.typ == "tag" {
		if target := tagTarget(newObj.data); target != "" {
			newCommit = target
		}
	}
	if cmd.old == zeroOID {
		if exists {
			return false, "already exists"
		}
		r.refs[cmd.ref] = cmd.new
		r.version++
		return true, ""
	}
	if !exists || cur != cmd.old {
		return false, "remote ref is at " + cur + " but expected " + cmd.old
	}
	if !r.isAncestor(cmd.old, newCommit) {
		return false, "non-fast-forward"
	}
	r.refs[cmd.ref] = cmd.new
	r.version++
	return true, ""
}
