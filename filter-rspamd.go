//
// Copyright (c) 2019 Gilles Chehade <gilles@poolp.org>
//
// Permission to use, copy, modify, and distribute this software for any
// purpose with or without fee is hereby granted, provided that the above
// copyright notice and this permission notice appear in all copies.
//
// THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
// WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
// MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
// ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
// WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
// ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
// OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
//

package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"log"
	"net/http"
	"encoding/json"
)

type session struct {
	id string

	rdns string
	fcrdns string
	src string
	dst string
	heloName string

	msgid string
	mail_from string
	rcpt_to []string
	message []string

	action string;
}

type rspamd struct {
	Score         float32
	RequiredScore float32 `json:"required_score"`
	Subject       string
	Action        string
	DKIMSig       string `json:"dkim-signature"`
}

var sessions = make(map[string]session)

var reporters = map[string]func(string, []string) {
	"link-connect": linkConnect,
	"link-disconnect": linkDisconnect,
	"link-identify": linkIdentify,
	"link-reset": linkReset,
	"tx-begin": txBegin,
	"tx-mail": txMail,
	"tx-rcpt": txRcpt,
}

var filters = map[string]func(string, []string) {
	"data-line": dataLine,
	"commit": dataCommit,
}

func linkConnect(sessionId string, params []string) {
	if len(params) != 4 {
		log.Fatal("invalid input, shouldn't happen")
	}

	s := session{}
	s.id = sessionId
	s.rdns = params[0]
	s.fcrdns = params[1]
	s.src = params[2]
	s.dst = params[3]
	sessions[s.id] = s
}

func linkDisconnect(sessionId string, params []string) {
	if len(params) != 0 {
		log.Fatal("invalid input, shouldn't happen")
	}
	delete(sessions, sessionId)
}

func linkIdentify(sessionId string, params []string) {
	if len(params) != 2 {
		log.Fatal("invalid input, shouldn't happen")
	}

	s := sessions[sessionId]
	s.heloName = params[1]
	sessions[s.id] = s
}

func linkReset(sessionId string, params []string) {
	if len(params) != 0 {
		log.Fatal("invalid input, shouldn't happen")
	}

	s := sessions[sessionId]
	s.msgid = ""
	s.mail_from = ""
	s.rcpt_to = nil
	s.message = nil
	s.action  = "";	
	sessions[s.id] = s
}

func txBegin(sessionId string, params []string) {
	if len(params) != 1 {
		log.Fatal("invalid input, shouldn't happen")
	}

	s := sessions[sessionId]
	s.msgid = params[0]
	sessions[s.id] = s
}

func txMail(sessionId string, params []string) {
	if len(params) != 3 {
		log.Fatal("invalid input, shouldn't happen")
	}

	if params[2] != "ok" {
		return
	}

	s := sessions[sessionId]
	s.mail_from = params[1]
	sessions[s.id] = s
}

func txRcpt(sessionId string, params []string) {
	if len(params) != 3 {
		log.Fatal("invalid input, shouldn't happen")
	}

	if params[2] != "ok" {
		return
	}

	s := sessions[sessionId]
	s.rcpt_to = append(s.rcpt_to, params[1])
	sessions[s.id] = s
}

func dataLine(sessionId string, params []string) {
	if len (params) < 2 {
		log.Fatal("invalid input, shouldn't happen")
	}
	token := params[0]
	line := strings.Join(params[1:], "|")

	s := sessions[sessionId]
	if line == "." {
		go rspamdQuery(s, token)
		return
	}
	s.message = append(s.message, line)
	sessions[sessionId] = s
}

func dataCommit(sessionId string, params []string) {
	if len (params) != 2 {
		log.Fatal("invalid input, shouldn't happen")
	}

	token := params[0]
	s := sessions[sessionId]
	sessions[sessionId] = s

	
	switch s.action {
	case "reject":
		fmt.Printf("filter-result|%s|%s|reject|550 message rejected\n", token, sessionId)
	case "greylist":
		fmt.Printf("filter-result|%s|%s|reject|421 try again later\n", token, sessionId)
	case "soft reject":
		fmt.Printf("filter-result|%s|%s|reject|451 try again later\n", token, sessionId)
	default:
		fmt.Printf("filter-result|%s|%s|proceed\n", token, sessionId)
	}
}


func filterInit() {
	for k := range reporters {
		fmt.Printf("register|report|smtp-in|%s\n", k)
	}
	for k := range filters {
		fmt.Printf("register|filter|smtp-in|%s\n", k)
	}
	fmt.Println("register|ready")	
}

func flushMessage(s session, token string) {
	for _, line := range s.message {
		fmt.Printf("filter-dataline|%s|%s|%s\n", token, s.id, line)
	}
	fmt.Printf("filter-dataline|%s|%s|.\n", token, s.id)
}

func rspamdQuery(s session, token string) {
	r := strings.NewReader(strings.Join(s.message, "\n"))
	client := &http.Client{}
	req, err := http.NewRequest("POST", "http://localhost:11333/checkv2", r)
	if err != nil {
		flushMessage(s, token)
		return
	}

	req.Header.Add("Pass", "All")
	if !strings.HasPrefix(s.src, "unix:") {
		req.Header.Add("Ip", strings.Split(s.src, ":")[0])
	} else {
		req.Header.Add("Ip", "127.0.0.1")
	}
	
	req.Header.Add("Hostname", s.rdns)
	req.Header.Add("Helo", s.heloName)
	req.Header.Add("Queue-Id", s.msgid)
	req.Header.Add("From", s.mail_from)
	for _, rcpt_to := range s.rcpt_to {
		req.Header.Add("Rcpt", rcpt_to)
	}

	resp, err := client.Do(req)
	if err != nil {
		flushMessage(s, token)
		return
	}
	defer resp.Body.Close()

	rr := &rspamd{}
	if err := json.NewDecoder(resp.Body).Decode(rr); err != nil {
		flushMessage(s, token)
		return
	}

	switch rr.Action {
	case "reject":
		fallthrough
	case "greylist":
		fallthrough
	case "soft reject":
		s.action = rr.Action
		sessions[s.id] = s
		flushMessage(s, token)
		return
	}

	if rr.DKIMSig != "" {
		fmt.Printf("filter-dataline|%s|%s|%s: %s\n",
			token, s.id, "DKIM-Signature", rr.DKIMSig)
	}

	fmt.Printf("filter-dataline|%s|%s|%s: %s\n",
		token, s.id, "X-Spam-Action", rr.Action)

	if rr.Action == "add header" {
		fmt.Printf("filter-dataline|%s|%s|%s: %s\n",
			token, s.id, "X-Spam", "yes")
		fmt.Printf("filter-dataline|%s|%s|%s: %s\n",
			token, s.id, "X-Spam-Score",
			fmt.Sprintf("%v / %v",
				rr.Score, rr.RequiredScore))
	}

	inhdr := true
	for _, line := range s.message {
		if line == "" {
			inhdr = false
		}
		if rr.Action == "rewrite subject" && inhdr && strings.HasPrefix(line, "Subject: ") {
			fmt.Printf("filter-dataline|%s|%s|Subject: %s\n", token, s.id, rr.Subject)
		} else {
			fmt.Printf("filter-dataline|%s|%s|%s\n", token, s.id, line)
		}
	}
	fmt.Printf("filter-dataline|%s|%s|.\n", token, s.id)
	sessions[s.id] = s
}

func trigger(currentSlice map[string]func(string, []string), atoms []string) {
	found := false
	for k, v := range currentSlice {
		if k == atoms[4] {
			v(atoms[5], atoms[6:])
			found = true
			break
		}
	}
	if !found {
		os.Exit(1)
	}
}

func main() {
	filterInit()

	scanner := bufio.NewScanner(os.Stdin)
	for {
		if !scanner.Scan() {
			os.Exit(0)
		}

		
		atoms := strings.Split(scanner.Text(), "|")
		if len(atoms) < 6 {
			os.Exit(1)
		}

		switch atoms[0] {
		case "report":
			trigger(reporters, atoms)
		case "filter":
			trigger(filters, atoms)
		default:
			os.Exit(1)
		}
	}
}