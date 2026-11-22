/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

package opcua

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"time"
)

// GenerateIdentity makes a self-signed application instance certificate (OPC UA Part 6,
// 6.2.2) and its 2048-bit RSA key: the application URI as a URI subject alternative name,
// hosts (names or IP addresses) as DNS and IP names, valid for validity from an hour ago.
// The certificate is DER.
func GenerateIdentity(applicationURI, applicationName string, hosts []string, validity time.Duration) ([]byte, *rsa.PrivateKey, error) {
	uri, err := url.Parse(applicationURI)
	if err != nil || uri.Scheme == "" {
		return nil, nil, fmt.Errorf("application URI %q is not a URI", applicationURI)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	publicKey := x509.MarshalPKCS1PublicKey(&key.PublicKey)
	keyId := sha1.Sum(publicKey)
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: applicationName, Organization: []string{"ApiaryTech"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(validity),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageContentCommitment | x509.KeyUsageKeyEncipherment |
			x509.KeyUsageDataEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		SubjectKeyId:          keyId[:],
		AuthorityKeyId:        keyId[:],
		URIs:                  []*url.URL{uri},
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else if host != "" {
			template.DNSNames = append(template.DNSNames, host)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	return der, key, nil
}
