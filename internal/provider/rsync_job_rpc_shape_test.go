package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRsyncSetResponseShapeDoesNotCrash pins the exact regression found via
// source review: Rsync.set()'s response has minute/hour/dayofmonth/month/
// dayofweek as comma-joined STRINGS (matching the config database's
// stored format), not arrays -- unlike Rsync.get(), which explode()s them
// into arrays before returning. Decoding a set() response directly into
// rsyncJobRPCObject (whose cron fields are []string) used to fail exactly
// like the memTotal/login bugs already fixed elsewhere in this client.
// Create()/Update() now only pull "uuid" out of set()'s response and
// re-fetch the full object via get() instead -- this test exercises that
// exact sequence against a fake server shaped like the real one.
func TestRsyncSetResponseShapeDoesNotCrash(t *testing.T) {
	var sawGetCall bool
	mux := http.NewServeMux()
	mux.HandleFunc("/rpc.php", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Service string `json:"service"`
			Method  string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		switch {
		case req.Service == "session" && req.Method == "login":
			_, _ = w.Write([]byte(`{"response": {"username": "admin", "status": "authenticated"}, "error": null}`))
		case req.Service == "Rsync" && req.Method == "set":
			// Realistic set() response: cron fields as comma-joined
			// strings (matching the datamodel's stored format), src/dest
			// only present as nested objects, no flat aliases.
			_, _ = w.Write([]byte(`{"response": {
				"uuid": "11111111-1111-1111-1111-111111111111",
				"enable": true,
				"sendemail": false,
				"comment": "test job",
				"type": "local",
				"mode": "push",
				"src": {"sharedfolderref": "aaaa", "uri": ""},
				"dest": {"sharedfolderref": "bbbb", "uri": ""},
				"minute": "0,30",
				"everynminute": false,
				"hour": "*",
				"everynhour": false,
				"month": "*",
				"dayofmonth": "*",
				"everyndayofmonth": false,
				"dayofweek": "*",
				"optionrecursive": true,
				"optiontimes": true,
				"optiongroup": true,
				"optionowner": true,
				"optioncompress": false,
				"optionarchive": true,
				"optiondelete": false,
				"optionquiet": true,
				"optionperms": true,
				"optionacls": false,
				"optionxattrs": false,
				"optiondryrun": false,
				"optionpartial": false,
				"extraoptions": "",
				"authentication": "password",
				"password": "",
				"sshcertificateref": "",
				"sshport": 22
			}, "error": null}`))
		case req.Service == "Rsync" && req.Method == "get":
			sawGetCall = true
			// Realistic get() response: cron fields as arrays, flat
			// src/dest aliases populated per type=local.
			_, _ = w.Write([]byte(`{"response": {
				"uuid": "11111111-1111-1111-1111-111111111111",
				"enable": true,
				"sendemail": false,
				"comment": "test job",
				"type": "local",
				"mode": "push",
				"srcsharedfolderref": "aaaa",
				"destsharedfolderref": "bbbb",
				"src": {"sharedfolderref": "aaaa", "uri": ""},
				"dest": {"sharedfolderref": "bbbb", "uri": ""},
				"minute": ["0", "30"],
				"everynminute": false,
				"hour": ["*"],
				"everynhour": false,
				"month": ["*"],
				"dayofmonth": ["*"],
				"everyndayofmonth": false,
				"dayofweek": ["*"],
				"optionrecursive": true,
				"optiontimes": true,
				"optiongroup": true,
				"optionowner": true,
				"optioncompress": false,
				"optionarchive": true,
				"optiondelete": false,
				"optionquiet": true,
				"optionperms": true,
				"optionacls": false,
				"optionxattrs": false,
				"optiondryrun": false,
				"optionpartial": false,
				"extraoptions": "",
				"authentication": "password",
				"password": "",
				"sshcertificateref": "",
				"sshport": 22
			}, "error": null}`))
		default:
			t.Fatalf("unexpected call: %s.%s", req.Service, req.Method)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := mustClientForHostPort(t, srv)

	// Mirror exactly what Create() does: set(), decode only the uuid, then
	// get() the full object.
	var created struct {
		UUID string `json:"uuid"`
	}
	if err := client.Call(context.Background(), "Rsync", "set", map[string]string{}, &created); err != nil {
		t.Fatalf("set() call failed (this is the regression this test guards against): %v", err)
	}
	if created.UUID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("unexpected uuid from set(): %q", created.UUID)
	}

	var full rsyncJobRPCObject
	if err := client.Call(context.Background(), "Rsync", "get", map[string]string{"uuid": created.UUID}, &full); err != nil {
		t.Fatalf("get() call failed: %v", err)
	}
	if !sawGetCall {
		t.Fatal("expected a follow-up get() call")
	}
	if len(full.Minute) != 2 || full.Minute[0] != "0" || full.Minute[1] != "30" {
		t.Errorf("unexpected minute from get(): %v", full.Minute)
	}
	if full.SrcSharedFolderRef != "aaaa" || full.DestSharedFolderRef != "bbbb" {
		t.Errorf("unexpected src/dest aliases from get(): src=%q dest=%q", full.SrcSharedFolderRef, full.DestSharedFolderRef)
	}
}

// TestRsyncSetResponseWouldCrashIfDecodedDirectly documents (and locks in)
// why Create/Update don't decode set()'s response into rsyncJobRPCObject:
// doing so fails, exactly like the original bug report.
func TestRsyncSetResponseWouldCrashIfDecodedDirectly(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rpc.php", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Service string `json:"service"`
			Method  string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Service == "session" && req.Method == "login" {
			_, _ = w.Write([]byte(`{"response": {"username": "admin", "status": "authenticated"}, "error": null}`))
			return
		}
		// A real set() response: "minute" is a string, not an array.
		_, _ = w.Write([]byte(`{"response": {"uuid": "x", "minute": "0,30"}, "error": null}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := mustClientForHostPort(t, srv)

	var direct rsyncJobRPCObject
	err := client.Call(context.Background(), "Rsync", "set", map[string]string{}, &direct)
	if err == nil {
		t.Fatal("expected decoding set()'s response directly into rsyncJobRPCObject to fail")
	}
}
