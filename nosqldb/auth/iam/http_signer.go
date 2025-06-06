// Copyright (c) 2016, 2025 Oracle and/or its affiliates. All rights reserved.

package iam

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPRequestSigner the interface to sign a request
type HTTPRequestSigner interface {
	Sign(r *http.Request) error
	ExpirationTime() time.Time
}

// KeyProvider interface that wraps information about the key's account owner
type KeyProvider interface {
	PrivateRSAKey() (*rsa.PrivateKey, error)
	KeyID() (string, error)
	ExpirationTime() time.Time
}

const signerVersion = "1"

// SignerBodyHashPredicate a function that allows to disable/enable body hashing
// of requests and headers associated with body content
type SignerBodyHashPredicate func(r *http.Request) bool

// ociRequestSigner implements the http-signatures-draft spec
// as described in https://tools.ietf.org/html/draft-cavage-http-signatures-08
type ociRequestSigner struct {
	KeyProvider    KeyProvider
	GenericHeaders []string
	BodyHeaders    []string
	ShouldHashBody SignerBodyHashPredicate
}

var (
	defaultGenericHeaders    = []string{"date", "(request-target)", "host"}
	defaultDelegationHeaders = []string{"date", "(request-target)", "host", "opc-obo-token"}
	defaultBodyHeaders       = []string{"content-length", "content-type", "x-content-sha256"}
	defaultBodyHashPredicate = func(r *http.Request) bool {
		// Has the body if explicitly told to
		if r.Header.Get("X-Nosql-Hash-Body") == "true" {
			return true
		}
		// Otherwise only hash if one of the following request types
		return r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch
	}
)

// DefaultGenericHeaders list of default generic headers that is used in signing
func DefaultGenericHeaders() []string {
	return makeACopy(defaultGenericHeaders)
}

// DefaultDelegationHeaders list of default headers that is used in signing with
// delegation token
func DefaultDelegationHeaders() []string {
	return makeACopy(defaultDelegationHeaders)
}

// DefaultBodyHeaders list of default body headers that is used in signing
func DefaultBodyHeaders() []string {
	return makeACopy(defaultBodyHeaders)
}

// DefaultRequestSigner creates a signer with default parameters.
func DefaultRequestSigner(provider KeyProvider) HTTPRequestSigner {
	return RequestSigner(provider, defaultGenericHeaders, defaultBodyHeaders)
}

// DelegationRequestSigner creates a signer with parameters including delegation token.
func DelegationRequestSigner(provider KeyProvider) HTTPRequestSigner {
	return RequestSigner(provider, defaultDelegationHeaders, defaultBodyHeaders)
}

// RequestSignerExcludeBody creates a signer without hash the body.
func RequestSignerExcludeBody(provider KeyProvider) HTTPRequestSigner {
	bodyHashPredicate := func(r *http.Request) bool {
		// weak request signer will not hash the body unless explicitly told to
		return r.Header.Get("X-Nosql-Hash-Body") == "true"
	}
	return RequestSignerWithBodyHashingPredicate(provider, defaultGenericHeaders, defaultBodyHeaders, bodyHashPredicate)
}

// DelegationRequestSignerExcludeBody creates a signer without hash the body but including delegation token.
func DelegationRequestSignerExcludeBody(provider KeyProvider) HTTPRequestSigner {
	bodyHashPredicate := func(r *http.Request) bool {
		// weak request signer will not hash the body unless explicitly told to
		return r.Header.Get("X-Nosql-Hash-Body") == "true"
	}
	return RequestSignerWithBodyHashingPredicate(provider, defaultDelegationHeaders, defaultBodyHeaders, bodyHashPredicate)
}

// NewSignerFromOCIRequestSigner creates a copy of the request signer and attaches the new SignerBodyHashPredicate
// returns an error if the passed signer is not of type ociRequestSigner
func NewSignerFromOCIRequestSigner(oldSigner HTTPRequestSigner, predicate SignerBodyHashPredicate) (HTTPRequestSigner, error) {
	if oldS, ok := oldSigner.(ociRequestSigner); ok {
		s := ociRequestSigner{
			KeyProvider:    oldS.KeyProvider,
			GenericHeaders: oldS.GenericHeaders,
			BodyHeaders:    oldS.BodyHeaders,
			ShouldHashBody: predicate,
		}
		return s, nil

	}
	return nil, fmt.Errorf("can not create a signer, input signer needs to be of type ociRequestSigner")
}

// RequestSigner creates a signer that utilizes the specified headers for signing
// and the default predicate for using the body of the request as part of the signature
func RequestSigner(provider KeyProvider, genericHeaders, bodyHeaders []string) HTTPRequestSigner {
	return ociRequestSigner{
		KeyProvider:    provider,
		GenericHeaders: genericHeaders,
		BodyHeaders:    bodyHeaders,
		ShouldHashBody: defaultBodyHashPredicate}
}

// RequestSignerWithBodyHashingPredicate creates a signer that utilizes the specified headers for signing, as well as a predicate for using
// the body of the request and bodyHeaders parameter as part of the signature
func RequestSignerWithBodyHashingPredicate(provider KeyProvider, genericHeaders, bodyHeaders []string, shouldHashBody SignerBodyHashPredicate) HTTPRequestSigner {
	return ociRequestSigner{
		KeyProvider:    provider,
		GenericHeaders: genericHeaders,
		BodyHeaders:    bodyHeaders,
		ShouldHashBody: shouldHashBody}
}

func (signer ociRequestSigner) getSigningHeaders(r *http.Request) []string {
	var result []string
	result = append(result, signer.GenericHeaders...)

	if signer.ShouldHashBody(r) {
		result = append(result, signer.BodyHeaders...)
	}

	return result
}

func (signer ociRequestSigner) ExpirationTime() time.Time {
	if signer.KeyProvider == nil {
		return time.Now().Add(-time.Second)
	}
	return signer.KeyProvider.ExpirationTime()
}

func (signer ociRequestSigner) getSigningString(request *http.Request) string {
	signingHeaders := signer.getSigningHeaders(request)
	signingParts := make([]string, len(signingHeaders))
	for i, part := range signingHeaders {
		var value string
		part = strings.ToLower(part)
		switch part {
		case "(request-target)":
			value = getRequestTarget(request)
		case "host":
			value = request.URL.Host
			if len(value) == 0 {
				value = request.Host
			}
		default:
			value = request.Header.Get(part)
		}
		signingParts[i] = fmt.Sprintf("%s: %s", part, value)
	}

	signingString := strings.Join(signingParts, "\n")
	return signingString

}

func getRequestTarget(request *http.Request) string {
	lowercaseMethod := strings.ToLower(request.Method)
	return fmt.Sprintf("%s %s", lowercaseMethod, request.URL.RequestURI())
}

func calculateHashOfBody(request *http.Request) (err error) {
	var hash string
	hash, err = GetBodyHash(request)
	if err != nil {
		return
	}
	request.Header.Set("X-Content-SHA256", hash)
	return
}

// drainBody reads all of b to memory and then returns two equivalent
// ReadClosers yielding the same bytes.
//
// It returns an error if the initial slurp of all bytes fails. It does not attempt
// to make the returned ReadClosers have identical error-matching behavior.
func drainBody(b io.ReadCloser) (r1, r2 io.ReadCloser, err error) {
	if b == http.NoBody {
		// No copying needed. Preserve the magic sentinel meaning of NoBody.
		return http.NoBody, http.NoBody, nil
	}
	var buf bytes.Buffer
	if _, err = buf.ReadFrom(b); err != nil {
		return nil, b, err
	}
	if err = b.Close(); err != nil {
		return nil, b, err
	}
	return io.NopCloser(&buf), io.NopCloser(bytes.NewReader(buf.Bytes())), nil
}

func hashAndEncode(data []byte) string {
	hashedContent := sha256.Sum256(data)
	hash := base64.StdEncoding.EncodeToString(hashedContent[:])
	return hash
}

// GetBodyHash creates a base64 string from the hash of body the request
func GetBodyHash(request *http.Request) (hashString string, err error) {
	if request.Body == nil {
		request.ContentLength = 0
		request.Header.Set("Content-Length", fmt.Sprintf("%v", request.ContentLength))
		return hashAndEncode([]byte("")), nil
	}

	var data []byte
	var bReader io.ReadCloser
	bReader, request.Body, err = drainBody(request.Body)
	if err != nil {
		return "", fmt.Errorf("can not read body of request while calculating body hash: %s", err.Error())
	}

	data, err = io.ReadAll(bReader)
	if err != nil {
		return "", fmt.Errorf("can not read body of request while calculating body hash: %s", err.Error())
	}

	// Since the request can be coming from a binary body. Make an attempt to set the body length
	request.ContentLength = int64(len(data))
	request.Header.Set("Content-Length", fmt.Sprintf("%v", request.ContentLength))

	hashString = hashAndEncode(data)
	return
}

func (signer ociRequestSigner) computeSignature(request *http.Request) (signature string, err error) {
	signingString := signer.getSigningString(request)
	hasher := sha256.New()
	hasher.Write([]byte(signingString))
	hashed := hasher.Sum(nil)

	privateKey, err := signer.KeyProvider.PrivateRSAKey()
	if err != nil {
		return
	}

	var unencodedSig []byte
	unencodedSig, e := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, hashed)
	if e != nil {
		err = fmt.Errorf("can not compute signature while signing the request %s: ", e.Error())
		return
	}

	signature = base64.StdEncoding.EncodeToString(unencodedSig)
	return
}

// Sign signs the http request, by inspecting the necessary headers. Once signed
// the request will have the proper 'Authorization' header set, otherwise
// an error is returned
func (signer ociRequestSigner) Sign(request *http.Request) (err error) {
	if signer.ShouldHashBody(request) {
		err = calculateHashOfBody(request)
		if err != nil {
			return
		}
	}

	var signature string
	if signature, err = signer.computeSignature(request); err != nil {
		return
	}

	signingHeaders := strings.Join(signer.getSigningHeaders(request), " ")

	var keyID string
	if keyID, err = signer.KeyProvider.KeyID(); err != nil {
		return
	}

	authValue := fmt.Sprintf("Signature version=\"%s\",headers=\"%s\",keyId=\"%s\",algorithm=\"rsa-sha256\",signature=\"%s\"",
		signerVersion, signingHeaders, keyID, signature)

	request.Header.Set("Authorization", authValue)

	return
}
