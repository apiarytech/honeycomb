/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

// The security of an OPC UA secure channel (OPC UA Part 6, 6.7), from both ends: the same code
// signs and encrypts for the client in ApiaryTech's PLC4Go fork (internal/opcua), written for
// both and kept in step with it.

package opcua

import (
	"bytes"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"strings"
	"sync"

	readWriteModel "github.com/apache/plc4x/plc4go/protocols/opcua/readwrite/model"
)

// Sizes of the parts of a chunk that are not the message body (OPC UA Part 6, 6.7.2).
const (
	secureMessageHeaderSize     = 12 // message type, chunk type, size, secure channel id
	symmetricSecurityHeaderSize = 4  // token id
	sequenceHeaderSize          = 8  // sequence number, request id
	chunkOverhead               = secureMessageHeaderSize + symmetricSecurityHeaderSize + sequenceHeaderSize
)

// cryptoRandRead fills nonces; a variable so tests can make them predictable.
var cryptoRandRead = rand.Read

// SECURITY_POLICY_URI_PREFIX starts every security policy URI.
const SECURITY_POLICY_URI_PREFIX = "http://opcfoundation.org/UA/SecurityPolicy#"

// asymmetricSignature names an RSA signature scheme of a security policy.
type asymmetricSignature int

const (
	rsaPkcs15Sha1 asymmetricSignature = iota
	rsaPkcs15Sha256
	rsaPssSha256
)

// asymmetricEncryption names an RSA encryption scheme of a security policy.
type asymmetricEncryption int

const (
	rsaPkcs15 asymmetricEncryption = iota
	rsaOaepSha1
	rsaOaepSha256
)

// securityPolicy holds the algorithms and sizes of an OPC UA security policy (OPC UA Part 7).
// The asymmetric algorithms secure OpenSecureChannel messages and the session handshake; the
// symmetric ones, keyed from both sides' nonces, secure every other message.
type securityPolicy struct {
	name string
	uri  string

	asymmetricSignature  asymmetricSignature
	asymmetricEncryption asymmetricEncryption
	// symmetricHash is the hash of the HMAC that signs messages and of the P_hash that derives keys.
	symmetricHash func() hash.Hash
	// signatureKeyLength, encryptionKeyLength and the AES block size are the parts of the
	// derived key material, in this order.
	signatureKeyLength  int
	encryptionKeyLength int
	nonceLength         int
}

// SECURITY_POLICY_NONE_URI is the URI of the policy without security.
const SECURITY_POLICY_NONE_URI = SECURITY_POLICY_URI_PREFIX + "None"

var securityPolicies = []*securityPolicy{
	{name: "None", uri: SECURITY_POLICY_NONE_URI},
	{
		name: "Basic128Rsa15", uri: SECURITY_POLICY_URI_PREFIX + "Basic128Rsa15",
		asymmetricSignature: rsaPkcs15Sha1, asymmetricEncryption: rsaPkcs15, symmetricHash: sha1.New,
		signatureKeyLength: 16, encryptionKeyLength: 16, nonceLength: 16,
	},
	{
		name: "Basic256", uri: SECURITY_POLICY_URI_PREFIX + "Basic256",
		asymmetricSignature: rsaPkcs15Sha1, asymmetricEncryption: rsaOaepSha1, symmetricHash: sha1.New,
		signatureKeyLength: 24, encryptionKeyLength: 32, nonceLength: 32,
	},
	{
		name: "Basic256Sha256", uri: SECURITY_POLICY_URI_PREFIX + "Basic256Sha256",
		asymmetricSignature: rsaPkcs15Sha256, asymmetricEncryption: rsaOaepSha1, symmetricHash: sha256.New,
		signatureKeyLength: 32, encryptionKeyLength: 32, nonceLength: 32,
	},
	{
		name: "Aes128_Sha256_RsaOaep", uri: SECURITY_POLICY_URI_PREFIX + "Aes128_Sha256_RsaOaep",
		asymmetricSignature: rsaPkcs15Sha256, asymmetricEncryption: rsaOaepSha1, symmetricHash: sha256.New,
		signatureKeyLength: 32, encryptionKeyLength: 16, nonceLength: 32,
	},
	{
		name: "Aes256_Sha256_RsaPss", uri: SECURITY_POLICY_URI_PREFIX + "Aes256_Sha256_RsaPss",
		asymmetricSignature: rsaPssSha256, asymmetricEncryption: rsaOaepSha256, symmetricHash: sha256.New,
		signatureKeyLength: 32, encryptionKeyLength: 32, nonceLength: 32,
	},
}

// securityPolicyByName finds a policy by its name ("Basic256Sha256"), ignoring case, or by its URI.
func securityPolicyByName(name string) (*securityPolicy, error) {
	if name == "" {
		name = "None"
	}
	for _, policy := range securityPolicies {
		if strings.EqualFold(policy.name, name) || policy.uri == name {
			return policy, nil
		}
	}
	names := make([]string, len(securityPolicies))
	for i, policy := range securityPolicies {
		names[i] = policy.name
	}
	return nil, fmt.Errorf("unknown security-policy %q: use one of %s", name, strings.Join(names, ", "))
}

// isNone reports whether the policy is None: no signatures, no encryption.
func (p *securityPolicy) isNone() bool {
	return p.symmetricHash == nil
}

// symmetricSignatureSize is the size of the HMAC that signs a symmetric chunk.
func (p *securityPolicy) symmetricSignatureSize() int {
	if p.isNone() {
		return 0
	}
	return p.symmetricHash().Size()
}

// symmetricBlockSize is the AES block size.
func (p *securityPolicy) symmetricBlockSize() int {
	if p.isNone() {
		return 1
	}
	return aes.BlockSize
}

// asymmetricSignatureURI names the policy's RSA signature scheme in a SignatureData.
func (p *securityPolicy) asymmetricSignatureURI() string {
	switch p.asymmetricSignature {
	case rsaPkcs15Sha1:
		return "http://www.w3.org/2000/09/xmldsig#rsa-sha1"
	case rsaPssSha256:
		return "http://opcfoundation.org/UA/security/rsa-pss-sha2-256"
	default:
		return "http://www.w3.org/2001/04/xmldsig-more#rsa-sha256"
	}
}

// asymmetricEncryptionURI names the policy's RSA encryption scheme in a user identity token.
func (p *securityPolicy) asymmetricEncryptionURI() string {
	switch p.asymmetricEncryption {
	case rsaPkcs15:
		return "http://www.w3.org/2001/04/xmlenc#rsa-1_5"
	case rsaOaepSha256:
		return "http://opcfoundation.org/UA/security/rsa-oaep-sha2-256"
	default:
		return "http://www.w3.org/2001/04/xmlenc#rsa-oaep"
	}
}

// digest hashes data with the hash of the policy's RSA signature scheme.
func (p *securityPolicy) digest(data []byte) (crypto.Hash, []byte) {
	if p.asymmetricSignature == rsaPkcs15Sha1 {
		sum := sha1.Sum(data)
		return crypto.SHA1, sum[:]
	}
	sum := sha256.Sum256(data)
	return crypto.SHA256, sum[:]
}

// asymmetricSign signs data with the policy's RSA signature scheme.
func (p *securityPolicy) asymmetricSign(key *rsa.PrivateKey, data []byte) ([]byte, error) {
	hashFunction, digest := p.digest(data)
	if p.asymmetricSignature == rsaPssSha256 {
		return rsa.SignPSS(rand.Reader, key, hashFunction, digest, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	}
	return rsa.SignPKCS1v15(rand.Reader, key, hashFunction, digest)
}

// asymmetricVerify checks a signature made with the policy's RSA signature scheme.
func (p *securityPolicy) asymmetricVerify(key *rsa.PublicKey, data, signature []byte) error {
	hashFunction, digest := p.digest(data)
	if p.asymmetricSignature == rsaPssSha256 {
		return rsa.VerifyPSS(key, hashFunction, digest, signature, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	}
	return rsa.VerifyPKCS1v15(key, hashFunction, digest, signature)
}

// asymmetricPlainTextBlockSize is how many bytes of plain text one RSA block of key carries.
func (p *securityPolicy) asymmetricPlainTextBlockSize(key *rsa.PublicKey) int {
	switch p.asymmetricEncryption {
	case rsaPkcs15:
		return key.Size() - 11
	case rsaOaepSha256:
		return key.Size() - 2*sha256.Size - 2
	default:
		return key.Size() - 2*sha1.Size - 2
	}
}

// asymmetricEncrypt encrypts one block of plain text for key.
func (p *securityPolicy) asymmetricEncrypt(key *rsa.PublicKey, block []byte) ([]byte, error) {
	switch p.asymmetricEncryption {
	case rsaPkcs15:
		return rsa.EncryptPKCS1v15(rand.Reader, key, block)
	case rsaOaepSha256:
		return rsa.EncryptOAEP(sha256.New(), rand.Reader, key, block, nil)
	default:
		return rsa.EncryptOAEP(sha1.New(), rand.Reader, key, block, nil)
	}
}

// asymmetricDecrypt decrypts one block of cipher text with key.
func (p *securityPolicy) asymmetricDecrypt(key *rsa.PrivateKey, block []byte) ([]byte, error) {
	switch p.asymmetricEncryption {
	case rsaPkcs15:
		return rsa.DecryptPKCS1v15(nil, key, block)
	case rsaOaepSha256:
		return rsa.DecryptOAEP(sha256.New(), nil, key, block, nil)
	default:
		return rsa.DecryptOAEP(sha1.New(), nil, key, block, nil)
	}
}

// asymmetricEncryptAll encrypts plainText for key, block by block.
func (p *securityPolicy) asymmetricEncryptAll(key *rsa.PublicKey, plainText []byte) ([]byte, error) {
	blockSize := p.asymmetricPlainTextBlockSize(key)
	cipherText := make([]byte, 0, (len(plainText)+blockSize-1)/blockSize*key.Size())
	for start := 0; start < len(plainText); start += blockSize {
		block, err := p.asymmetricEncrypt(key, plainText[start:min(start+blockSize, len(plainText))])
		if err != nil {
			return nil, fmt.Errorf("error encrypting: %w", err)
		}
		cipherText = append(cipherText, block...)
	}
	return cipherText, nil
}

// keySet is one side's key material of a secure channel token (OPC UA Part 6, 6.7.5).
type keySet struct {
	signingKey    []byte
	encryptingKey []byte
	iv            []byte
}

// deriveKeys derives one side's keys. The keys a side secures its messages with come from
// P_hash(secret = the other side's nonce, seed = its own nonce).
func (p *securityPolicy) deriveKeys(secret, seed []byte) keySet {
	length := p.signatureKeyLength + p.encryptionKeyLength + aes.BlockSize
	material := pHash(p.symmetricHash, secret, seed, length)
	return keySet{
		signingKey:    material[:p.signatureKeyLength],
		encryptingKey: material[p.signatureKeyLength : p.signatureKeyLength+p.encryptionKeyLength],
		iv:            material[p.signatureKeyLength+p.encryptionKeyLength:],
	}
}

// pHash is the TLS P_hash function (RFC 5246, section 5) OPC UA derives its keys with.
func pHash(hashFunction func() hash.Hash, secret, seed []byte, length int) []byte {
	result := make([]byte, 0, length)
	mac := hmac.New(hashFunction, secret)
	mac.Write(seed)
	a := mac.Sum(nil) // A(1)
	for len(result) < length {
		mac.Reset()
		mac.Write(a)
		mac.Write(seed)
		result = mac.Sum(result)
		mac.Reset()
		mac.Write(a)
		a = mac.Sum(nil)
	}
	return result[:length]
}

// symmetricSign signs data with the HMAC of the policy.
func (p *securityPolicy) symmetricSign(keys keySet, data []byte) []byte {
	mac := hmac.New(p.symmetricHash, keys.signingKey)
	mac.Write(data)
	return mac.Sum(nil)
}

// symmetricVerify checks an HMAC signature in constant time.
func (p *securityPolicy) symmetricVerify(keys keySet, data, signature []byte) error {
	if subtle.ConstantTimeCompare(p.symmetricSign(keys, data), signature) != 1 {
		return errors.New("message signature does not verify")
	}
	return nil
}

// symmetricCrypt encrypts or decrypts data in place with AES-CBC. len(data) must be a
// multiple of the AES block size.
func (p *securityPolicy) symmetricCrypt(keys keySet, data []byte, encrypt bool) error {
	if len(data)%aes.BlockSize != 0 {
		return fmt.Errorf("%d bytes are not a whole number of AES blocks", len(data))
	}
	block, err := aes.NewCipher(keys.encryptingKey)
	if err != nil {
		return fmt.Errorf("error creating cipher: %w", err)
	}
	if encrypt {
		cipher.NewCBCEncrypter(block, keys.iv).CryptBlocks(data, data)
	} else {
		cipher.NewCBCDecrypter(block, keys.iv).CryptBlocks(data, data)
	}
	return nil
}

// channelSecurity signs, encrypts, verifies and decrypts the chunks of one secure channel
// (OPC UA Part 6, 6.7). It works on whole serialized chunks: OpenSecureChannel chunks with the
// asymmetric algorithms of the policy, every other chunk with the symmetric keys derived from
// both sides' nonces.
//
// "Local" is the side that holds this value, "remote" its peer: the driver is the local side
// of its channels, and a test can build the server's side the same way.
type channelSecurity struct {
	policy *securityPolicy
	mode   readWriteModel.MessageSecurityMode

	localKey          *rsa.PrivateKey
	localCertificate  []byte // DER
	remoteCertificate *x509.Certificate

	mutex sync.RWMutex
	// localNonce is the nonce of the OpenSecureChannel request in flight; tokens holds the
	// keys of the current token and of the one before it, which the remote side may still
	// use for a while after a renewal.
	localNonce   []byte
	currentToken uint32
	tokens       map[uint32]tokenKeys
}

// tokenKeys are the keys of one security token: the local side secures with local and
// checks what it receives with remote.
type tokenKeys struct {
	local, remote keySet
}

// newChannelSecurity builds the security of a channel. For a policy other than None and a mode
// other than None it needs the local key and certificate and the remote certificate.
func newChannelSecurity(policy *securityPolicy, mode readWriteModel.MessageSecurityMode, localKey *rsa.PrivateKey, localCertificate []byte, remoteCertificate *x509.Certificate) (*channelSecurity, error) {
	c := &channelSecurity{
		policy:            policy,
		mode:              mode,
		localKey:          localKey,
		localCertificate:  localCertificate,
		remoteCertificate: remoteCertificate,
		tokens:            map[uint32]tokenKeys{},
	}
	if policy.isNone() {
		c.mode = readWriteModel.MessageSecurityMode_messageSecurityModeNone
		return c, nil
	}
	if c.mode == readWriteModel.MessageSecurityMode_messageSecurityModeNone {
		return nil, fmt.Errorf("security policy %s needs message security Sign or SignAndEncrypt", policy.name)
	}
	if localKey == nil || len(localCertificate) == 0 {
		return nil, fmt.Errorf("security policy %s needs a certificate and key", policy.name)
	}
	if remoteCertificate == nil {
		return nil, fmt.Errorf("security policy %s needs the remote certificate", policy.name)
	}
	if _, ok := remoteCertificate.PublicKey.(*rsa.PublicKey); !ok {
		return nil, fmt.Errorf("remote certificate has a %T key, the security policies need RSA", remoteCertificate.PublicKey)
	}
	return c, nil
}

// secured reports whether chunks are signed (and maybe encrypted).
func (c *channelSecurity) secured() bool {
	return c.mode != readWriteModel.MessageSecurityMode_messageSecurityModeNone
}

// encrypted reports whether symmetric chunks are encrypted as well as signed.
func (c *channelSecurity) encrypted() bool {
	return c.mode == readWriteModel.MessageSecurityMode_messageSecurityModeSignAndEncrypt
}

func (c *channelSecurity) remoteKey() *rsa.PublicKey {
	return c.remoteCertificate.PublicKey.(*rsa.PublicKey)
}

// localThumbprint is the SHA-1 thumbprint of the local certificate.
func (c *channelSecurity) localThumbprint() []byte {
	if len(c.localCertificate) == 0 {
		return nil
	}
	sum := sha1.Sum(c.localCertificate)
	return sum[:]
}

// remoteThumbprint is the SHA-1 thumbprint of the remote certificate, which an
// OpenSecureChannel request names as its receiver.
func (c *channelSecurity) remoteThumbprint() []byte {
	if c.remoteCertificate == nil {
		return nil
	}
	sum := sha1.Sum(c.remoteCertificate.Raw)
	return sum[:]
}

// localCertificateIfSecured is the local certificate an OpenSecureChannel or CreateSession
// request carries: none without security.
func (c *channelSecurity) localCertificateIfSecured() []byte {
	if !c.secured() {
		return nil
	}
	return c.localCertificate
}

// remoteThumbprintIfSecured is the thumbprint an OpenSecureChannel request names its receiver
// by: none without security.
func (c *channelSecurity) remoteThumbprintIfSecured() []byte {
	if !c.secured() {
		return nil
	}
	return c.remoteThumbprint()
}

// newLocalNonce makes the nonce of the next OpenSecureChannel request; nil without security.
func (c *channelSecurity) newLocalNonce() ([]byte, error) {
	if !c.secured() {
		return nil, nil
	}
	nonce := make([]byte, c.policy.nonceLength)
	if _, err := cryptoRandRead(nonce); err != nil {
		return nil, fmt.Errorf("error making nonce: %w", err)
	}
	c.mutex.Lock()
	c.localNonce = nonce
	c.mutex.Unlock()
	return nonce, nil
}

// installToken derives the keys of a token from the nonce of the request that asked for it and
// the remote side's nonce, and makes it the current token. The token before it stays known,
// for chunks the remote side secured before it switched.
func (c *channelSecurity) installToken(tokenId uint32, remoteNonce []byte) error {
	if !c.secured() {
		return nil
	}
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if len(remoteNonce) != c.policy.nonceLength {
		return fmt.Errorf("remote nonce has %d bytes, security policy %s needs %d", len(remoteNonce), c.policy.name, c.policy.nonceLength)
	}
	if len(c.localNonce) != c.policy.nonceLength {
		return errors.New("no local nonce for the token")
	}
	keys := tokenKeys{
		local:  c.policy.deriveKeys(remoteNonce, c.localNonce),
		remote: c.policy.deriveKeys(c.localNonce, remoteNonce),
	}
	for id := range c.tokens {
		if id != c.currentToken {
			delete(c.tokens, id)
		}
	}
	c.tokens[tokenId] = keys
	c.currentToken = tokenId
	return nil
}

// Layout of a chunk: the message header (type, chunk type, size, channel id), the security
// header, then the sequence header and body, which is what gets padded, signed and encrypted.
const symmetricSecurityHeaderEnd = secureMessageHeaderSize + symmetricSecurityHeaderSize

// asymmetricHeader is the security header of an OpenSecureChannel chunk.
type asymmetricHeader struct {
	policyUri          string
	senderCertificate  []byte
	receiverThumbprint []byte
	end                int // offset of the sequence header
}

func parseAsymmetricHeader(chunk []byte) (asymmetricHeader, error) {
	var header asymmetricHeader
	offset := secureMessageHeaderSize
	field := func() ([]byte, error) {
		if len(chunk) < offset+4 {
			return nil, errors.New("chunk too short for its security header")
		}
		length := int32(binary.LittleEndian.Uint32(chunk[offset:]))
		offset += 4
		if length < 0 {
			return nil, nil
		}
		if len(chunk) < offset+int(length) {
			return nil, errors.New("chunk too short for its security header")
		}
		value := chunk[offset : offset+int(length)]
		offset += int(length)
		return value, nil
	}
	policyUri, err := field()
	if err != nil {
		return header, err
	}
	if header.senderCertificate, err = field(); err != nil {
		return header, err
	}
	if header.receiverThumbprint, err = field(); err != nil {
		return header, err
	}
	header.policyUri = string(policyUri)
	header.end = offset
	return header, nil
}

func setChunkSize(chunk []byte) {
	binary.LittleEndian.PutUint32(chunk[4:8], uint32(len(chunk)))
}

// appendPadding pads chunk so its part from start on, the padding and a signature of
// signatureSize fill whole blocks of blockSize. The padding is a PaddingSize byte, that many bytes of the same value,
// and, when the encrypting key is larger than 2048 bits (extraPadding), an ExtraPaddingSize byte
// with the high byte of the size.
func appendPadding(chunk []byte, start, blockSize, signatureSize int, extraPadding bool) []byte {
	overhead := 1
	if extraPadding {
		overhead = 2
	}
	padding := (blockSize - (len(chunk)-start+overhead+signatureSize)%blockSize) % blockSize
	low := byte(padding)
	for range padding + 1 {
		chunk = append(chunk, low)
	}
	if extraPadding {
		chunk = append(chunk, byte(padding>>8))
	}
	return chunk
}

// removePadding checks and strips the padding at the end of plainText (signature already
// removed). start is where the sequence header begins.
func removePadding(plainText []byte, start int, extraPadding bool) ([]byte, error) {
	end := len(plainText)
	overhead := 1
	if extraPadding {
		overhead = 2
	}
	if end-start < sequenceHeaderSize+overhead {
		return nil, errors.New("chunk too short for its padding")
	}
	low := plainText[end-overhead]
	padding := int(low)
	if extraPadding {
		padding |= int(plainText[end-1]) << 8
	}
	paddingStart := end - overhead - padding
	if paddingStart < start+sequenceHeaderSize {
		return nil, errors.New("malformed padding")
	}
	for _, b := range plainText[paddingStart : end-overhead+1] {
		if b != low {
			return nil, errors.New("malformed padding")
		}
	}
	return plainText[:paddingStart], nil
}

// secureAsymmetric signs and encrypts a serialized OpenSecureChannel chunk with the local
// private key and the remote public key. Without security it returns the chunk as it is.
func (c *channelSecurity) secureAsymmetric(chunk []byte) ([]byte, error) {
	if !c.secured() {
		return chunk, nil
	}
	header, err := parseAsymmetricHeader(chunk)
	if err != nil {
		return nil, err
	}
	remoteKey := c.remoteKey()
	plainBlock := c.policy.asymmetricPlainTextBlockSize(remoteKey)
	signatureSize := c.localKey.Size()

	out := make([]byte, 0, len(chunk)+plainBlock+signatureSize)
	out = append(out, chunk...)
	out = appendPadding(out, header.end, plainBlock, signatureSize, remoteKey.Size() > 256)
	plainTextSize := len(out) - header.end + signatureSize
	binary.LittleEndian.PutUint32(out[4:8], uint32(header.end+plainTextSize/plainBlock*remoteKey.Size()))
	signature, err := c.policy.asymmetricSign(c.localKey, out)
	if err != nil {
		return nil, fmt.Errorf("error signing: %w", err)
	}
	out = append(out, signature...)
	cipherText, err := c.policy.asymmetricEncryptAll(remoteKey, out[header.end:])
	if err != nil {
		return nil, err
	}
	return append(out[:header.end:header.end], cipherText...), nil
}

// openAsymmetric decrypts and verifies an OpenSecureChannel chunk from the remote side and
// returns it without padding and signature. It checks that the chunk uses the channel's
// policy, comes from the remote certificate and is meant for the local one.
func (c *channelSecurity) openAsymmetric(chunk []byte) ([]byte, error) {
	header, err := parseAsymmetricHeader(chunk)
	if err != nil {
		return nil, err
	}
	if header.policyUri != c.policy.uri {
		return nil, fmt.Errorf("message uses security policy %s, the channel uses %s", header.policyUri, c.policy.uri)
	}
	if !c.secured() {
		return chunk, nil
	}
	if !bytes.Equal(header.receiverThumbprint, c.localThumbprint()) {
		return nil, errors.New("OpenSecureChannel message is not meant for this side's certificate")
	}
	senderCertificates, err := x509.ParseCertificates(header.senderCertificate)
	if err != nil || len(senderCertificates) == 0 {
		return nil, fmt.Errorf("error parsing the sender certificate: %w", err)
	}
	if !bytes.Equal(senderCertificates[0].Raw, c.remoteCertificate.Raw) {
		return nil, errors.New("OpenSecureChannel message is signed with another certificate than the channel's")
	}
	blockSize := c.localKey.Size()
	cipherText := chunk[header.end:]
	if len(cipherText) == 0 || len(cipherText)%blockSize != 0 {
		return nil, fmt.Errorf("encrypted part of %d bytes is not a whole number of %d byte blocks", len(cipherText), blockSize)
	}
	plain := make([]byte, header.end, header.end+len(cipherText))
	copy(plain, chunk[:header.end])
	for start := 0; start < len(cipherText); start += blockSize {
		block, err := c.policy.asymmetricDecrypt(c.localKey, cipherText[start:start+blockSize])
		if err != nil {
			return nil, fmt.Errorf("error decrypting: %w", err)
		}
		plain = append(plain, block...)
	}
	signatureSize := c.remoteKey().Size()
	if len(plain)-header.end < signatureSize+sequenceHeaderSize {
		return nil, errors.New("chunk too short for its signature")
	}
	signed, signature := plain[:len(plain)-signatureSize], plain[len(plain)-signatureSize:]
	if err := c.policy.asymmetricVerify(c.remoteKey(), signed, signature); err != nil {
		return nil, fmt.Errorf("OpenSecureChannel signature does not verify: %w", err)
	}
	opened, err := removePadding(signed, header.end, c.localKey.Size() > 256)
	if err != nil {
		return nil, err
	}
	setChunkSize(opened)
	return opened, nil
}

// maxSymmetricBodySize is the largest message body a symmetric chunk of chunkSize bytes carries.
func (c *channelSecurity) maxSymmetricBodySize(chunkSize int) int {
	if !c.secured() {
		return chunkSize - chunkOverhead
	}
	signatureSize := c.policy.symmetricSignatureSize()
	if !c.encrypted() {
		return chunkSize - chunkOverhead - signatureSize
	}
	blockSize := c.policy.symmetricBlockSize()
	plainText := (chunkSize - symmetricSecurityHeaderEnd) / blockSize * blockSize
	return plainText - sequenceHeaderSize - signatureSize - 1
}

// secureSymmetric signs, and with SignAndEncrypt pads and encrypts, a serialized MSG or CLO
// chunk with the current token's keys. Without security it returns the chunk as it is.
func (c *channelSecurity) secureSymmetric(chunk []byte) ([]byte, error) {
	if !c.secured() {
		return chunk, nil
	}
	c.mutex.RLock()
	keys, ok := c.tokens[c.currentToken]
	c.mutex.RUnlock()
	if !ok {
		return nil, errors.New("no security token yet")
	}
	if len(chunk) < symmetricSecurityHeaderEnd+sequenceHeaderSize {
		return nil, errors.New("chunk too short")
	}
	signatureSize := c.policy.symmetricSignatureSize()
	out := make([]byte, 0, len(chunk)+c.policy.symmetricBlockSize()+signatureSize)
	out = append(out, chunk...)
	if c.encrypted() {
		out = appendPadding(out, symmetricSecurityHeaderEnd, c.policy.symmetricBlockSize(), signatureSize, false)
	}
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(out)+signatureSize))
	binary.LittleEndian.PutUint32(out[secureMessageHeaderSize:], c.currentToken)
	out = append(out, c.policy.symmetricSign(keys.local, out)...)
	if c.encrypted() {
		if err := c.policy.symmetricCrypt(keys.local, out[symmetricSecurityHeaderEnd:], true); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// openSymmetric decrypts and verifies a MSG or CLO chunk from the remote side with the keys of
// the token it names, and returns it without padding and signature.
func (c *channelSecurity) openSymmetric(chunk []byte) ([]byte, error) {
	if !c.secured() {
		return chunk, nil
	}
	if len(chunk) < symmetricSecurityHeaderEnd+sequenceHeaderSize {
		return nil, errors.New("chunk too short")
	}
	tokenId := binary.LittleEndian.Uint32(chunk[secureMessageHeaderSize:])
	c.mutex.RLock()
	keys, ok := c.tokens[tokenId]
	c.mutex.RUnlock()
	if !ok {
		return nil, fmt.Errorf("chunk uses unknown security token %d", tokenId)
	}
	opened := bytes.Clone(chunk)
	if c.encrypted() {
		if err := c.policy.symmetricCrypt(keys.remote, opened[symmetricSecurityHeaderEnd:], false); err != nil {
			return nil, err
		}
	}
	signatureSize := c.policy.symmetricSignatureSize()
	if len(opened)-symmetricSecurityHeaderEnd < signatureSize+sequenceHeaderSize {
		return nil, errors.New("chunk too short for its signature")
	}
	signed, signature := opened[:len(opened)-signatureSize], opened[len(opened)-signatureSize:]
	if err := c.policy.symmetricVerify(keys.remote, signed, signature); err != nil {
		return nil, err
	}
	if c.encrypted() {
		var err error
		if signed, err = removePadding(signed, symmetricSecurityHeaderEnd, false); err != nil {
			return nil, err
		}
	}
	setChunkSize(signed)
	return signed, nil
}

// signSession signs, for ActivateSession, the remote certificate followed by the remote
// side's session nonce (OPC UA Part 4, 5.6.3).
func (c *channelSecurity) signSession(remoteNonce []byte) (algorithm string, signature []byte, err error) {
	if !c.secured() {
		return "", nil, nil
	}
	data := append(bytes.Clone(c.remoteCertificate.Raw), remoteNonce...)
	signature, err = c.policy.asymmetricSign(c.localKey, data)
	if err != nil {
		return "", nil, fmt.Errorf("error signing: %w", err)
	}
	return c.policy.asymmetricSignatureURI(), signature, nil
}

// verifySession checks the server's CreateSession signature over the local certificate
// followed by the local session nonce.
func (c *channelSecurity) verifySession(localNonce []byte, algorithm string, signature []byte) error {
	if !c.secured() {
		return nil
	}
	if algorithm != c.policy.asymmetricSignatureURI() {
		return fmt.Errorf("session signed with %q, the security policy uses %q", algorithm, c.policy.asymmetricSignatureURI())
	}
	data := append(bytes.Clone(c.localCertificate), localNonce...)
	if err := c.policy.asymmetricVerify(c.remoteKey(), data, signature); err != nil {
		return fmt.Errorf("session signature does not verify: %w", err)
	}
	return nil
}
