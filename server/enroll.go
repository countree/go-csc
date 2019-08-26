/*-
 * Copyright 2016 Square Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"io/ioutil"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	sqlite3 "github.com/mattn/go-sqlite3"
	"golang.org/x/crypto/ssh"
)

const (
	sshHostCertificateType = 2
)

func (c *context) Enroll(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	hostname := vars["hostname"]

	if !clientAuthenticated(r) {
		http.Error(w, "no client certificate provided", http.StatusUnauthorized)
		return
	}
	if !clientHostnameMatches(hostname, r) {
		http.Error(w, "hostname does not match certificate", http.StatusForbidden)
		return
	}

	cert, err := c.EnrollHost(hostname, r)
	if err != nil {
		log.Print("internal error")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_, _ = w.Write([]byte(cert))
}

func (c *context) EnrollHost(hostname string, r *http.Request) (string, error) {
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return "", err
	}
	encodedPubkey := strings.TrimLeft(string(data), "\n")
	pubkey, _, _, _, err := ssh.ParseAuthorizedKey(data)
	if err != nil {
		return "", err
	}

	// Update table with host
	var result sql.Result
	if _, ok := c.db.Driver().(*sqlite3.SQLiteDriver); ok {
		// SQLite supports "insert or replace" for insert-or-update
		result, err = c.db.Exec(
			"INSERT OR REPLACE INTO hostkeys (hostname, pubkey) VALUES (?, ?)",
			encodedPubkey, hostname)
	} else {
		// MySQL supports "on duplicate key update" for insert-or-update
		result, err = c.db.Exec(
			"INSERT INTO hostkeys (hostname, pubkey) VALUES (?, ?) ON DUPLICATE KEY UPDATE pubkey = ?",
			hostname, encodedPubkey, encodedPubkey)
	}
	if err != nil {
		return "", err
	}

	id, err := result.LastInsertId()
	if err != nil {
		return "", err
	}

	signedCert, err := c.signHost(hostname, uint64(id), pubkey)
	if err != nil {
		return "", err
	}

	certString := base64.StdEncoding.EncodeToString(signedCert.Marshal())
	header := signedCert.Key.Type() + "-cert-v01@openssh.com "
	return header + certString, nil
}

func clientAuthenticated(r *http.Request) bool {
	return len(r.TLS.VerifiedChains) > 0
}

func clientHostnameMatches(hostname string, r *http.Request) bool {
	conn := r.TLS
	if len(conn.VerifiedChains) == 0 {
		return false
	}
	cert := conn.VerifiedChains[0][0]
	return cert.VerifyHostname(hostname) == nil
}

func (c *context) signHost(hostname string, serial uint64, pubkey ssh.PublicKey) (*ssh.Certificate, error) {
	nonce := make([]byte, 32)
	_, err := rand.Read(nonce)
	if err != nil {
		return nil, err
	}
	startTime := time.Now()
	week, err := time.ParseDuration(c.conf.CertDuration)
	if err != nil {
		return nil, err
	}
	endTime := startTime.Add(week)
	principals := []string{hostname}
	if c.conf.StripSuffix != "" && strings.HasSuffix(hostname, c.conf.StripSuffix) {
		principals = append(principals, strings.TrimSuffix(hostname, c.conf.StripSuffix))
	}
	if aliases, ok := c.conf.Aliases[hostname]; ok {
		principals = append(principals, aliases...)
	}
	template := ssh.Certificate{
		Nonce:           nonce,
		Key:             pubkey,
		Serial:          serial,
		CertType:        sshHostCertificateType,
		KeyId:           hostname,
		ValidPrincipals: principals,
		ValidAfter:      (uint64)(startTime.Unix()),
		ValidBefore:     (uint64)(endTime.Unix()),
	}

	err = template.SignCert(rand.Reader, c.signer)
	if err != nil {
		return nil, err
	}
	return &template, nil
}
