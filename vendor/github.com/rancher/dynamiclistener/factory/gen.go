package factory

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/rancher/dynamiclistener/cert"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
)

const (
	cnPrefix    = "listener.cattle.io/cn-"
	Static      = "listener.cattle.io/static"
	Fingerprint = "listener.cattle.io/fingerprint"
)

var (
	cnRegexp = regexp.MustCompile("^([A-Za-z0-9:][-A-Za-z0-9_.:]*)?[A-Za-z0-9:]$")
)

type TLS struct {
	CACert              []*x509.Certificate
	CAKey               crypto.Signer
	CN                  string
	Organization        []string
	FilterCN            func(...string) []string
	ExpirationDaysCheck int
}

func cns(secret *v1.Secret) (cns []string) {
	if secret == nil {
		return nil
	}
	for k, v := range secret.Annotations {
		if strings.HasPrefix(k, cnPrefix) {
			cns = append(cns, v)
		}
	}
	return
}

func collectCNs(secret *v1.Secret) (domains []string, ips []net.IP, err error) {
	var (
		cns = cns(secret)
	)

	sort.Strings(cns)

	for _, v := range cns {
		ip := net.ParseIP(v)
		if ip == nil {
			domains = append(domains, v)
		} else {
			ips = append(ips, ip)
		}
	}

	return
}

// Merge combines the SAN lists from the target and additional Secrets, and
// returns a potentially modified Secret, along with a bool indicating if the
// returned Secret is not the same as the target Secret. Secrets with expired
// certificates will never be returned.
//
// If the merge would not add any CNs to the additional Secret, the additional
// Secret is returned, to allow for certificate rotation/regeneration.
//
// If the merge would not add any CNs to the target Secret, the target Secret is
// returned; no merging is necessary.
//
// If neither certificate is acceptable as-is, a new certificate containing
// the union of the two lists is generated, using the private key from the
// first Secret. The returned Secret will contain the updated cert.
func (t *TLS) Merge(target, additional *v1.Secret) (*v1.Secret, bool, error) {
	// static secrets can't be altered, don't bother trying
	if IsStatic(target) {
		return target, false, nil
	}

	mergedCNs := append(cns(target), cns(additional)...)

	// if the additional secret already has all the CNs, use it in preference to the
	// current one. This behavior is required to allow for renewal or regeneration.
	if !NeedsUpdate(0, additional, mergedCNs...) && !t.IsExpired(additional) {
		return additional, true, nil
	}

	// if the target secret already has all the CNs, continue using it. The additional
	// cert had only a subset of the current CNs, so nothing needs to be added.
	if !NeedsUpdate(0, target, mergedCNs...) && !t.IsExpired(target) {
		return target, false, nil
	}

	// neither cert currently has all the necessary CNs or is unexpired; generate a new one.
	return t.generateCert(target, mergedCNs...)
}

// Renew returns a copy of the given certificate that has been re-signed
// to extend the NotAfter date. It is an error to attempt to renew
// a static (user-provided) certificate.
func (t *TLS) Renew(secret *v1.Secret) (*v1.Secret, error) {
	if IsStatic(secret) {
		return secret, cert.ErrStaticCert
	}
	cns := cns(secret)
	secret = secret.DeepCopy()
	secret.Annotations = map[string]string{}
	secret, _, err := t.generateCert(secret, cns...)
	return secret, err
}

// Filter ensures that the CNs are all valid accorting to both internal logic, and any filter callbacks.
// The returned list will contain only approved CN entries.
func (t *TLS) Filter(cn ...string) []string {
	if len(cn) == 0 || t.FilterCN == nil {
		return cn
	}
	return t.FilterCN(cn...)
}

// AddCN attempts to add a list of CN strings to a given Secret, returning the potentially-modified
// Secret along with a bool indicating whether or not it has been updated. The Secret will not be changed
// if it has an attribute indicating that it is static (aka user-provided), or if no new CNs were added.
func (t *TLS) AddCN(secret *v1.Secret, cn ...string) (*v1.Secret, bool, error) {
	cn = t.Filter(cn...)

	if IsStatic(secret) || !NeedsUpdate(0, secret, cn...) {
		return secret, false, nil
	}
	return t.generateCert(secret, cn...)
}

func (t *TLS) Regenerate(secret *v1.Secret) (*v1.Secret, error) {
	cns := cns(secret)
	secret, _, err := t.generateCert(nil, cns...)
	return secret, err
}

func (t *TLS) generateCert(secret *v1.Secret, cn ...string) (*v1.Secret, bool, error) {
	secret = secret.DeepCopy()
	if secret == nil {
		secret = &v1.Secret{}
	}

	if err := t.Verify(secret); err != nil {
		logrus.Warnf("unable to verify existing certificate: %v - signing operation may change certificate issuer", err)
	}

	secret = populateCN(secret, cn...)

	privateKey, err := getPrivateKey(secret)
	if err != nil {
		return nil, false, err
	}

	domains, ips, err := collectCNs(secret)
	if err != nil {
		return nil, false, err
	}

	newCert, err := t.newCert(domains, ips, privateKey)
	if err != nil {
		return nil, false, err
	}

	keyBytes, certBytes, err := MarshalChain(privateKey, append([]*x509.Certificate{newCert}, t.CACert...)...)
	if err != nil {
		return nil, false, err
	}

	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Type = v1.SecretTypeTLS
	secret.Data[v1.TLSCertKey] = certBytes
	secret.Data[v1.TLSPrivateKeyKey] = keyBytes
	secret.Annotations[Fingerprint] = fmt.Sprintf("SHA1=%X", sha1.Sum(newCert.Raw))

	return secret, true, nil
}

func (t *TLS) IsExpired(secret *v1.Secret) bool {
	certsPem := secret.Data[v1.TLSCertKey]
	if len(certsPem) == 0 {
		return false
	}

	certificates, err := cert.ParseCertsPEM(certsPem)
	if err != nil || len(certificates) == 0 {
		return false
	}

	expirationDays := time.Duration(t.ExpirationDaysCheck) * time.Hour * 24
	return time.Now().Add(expirationDays).After(certificates[0].NotAfter)
}

func (t *TLS) Verify(secret *v1.Secret) error {
	certsPem := secret.Data[v1.TLSCertKey]
	if len(certsPem) == 0 {
		return nil
	}

	certificates, err := cert.ParseCertsPEM(certsPem)
	if err != nil || len(certificates) == 0 {
		return err
	}

	verifyOpts := x509.VerifyOptions{
		Roots: x509.NewCertPool(),
		KeyUsages: []x509.ExtKeyUsage{
			x509.ExtKeyUsageAny,
		},
	}
	for _, c := range t.CACert {
		verifyOpts.Roots.AddCert(c)
	}

	_, err = certificates[0].Verify(verifyOpts)
	return err
}

func (t *TLS) newCert(domains []string, ips []net.IP, privateKey crypto.Signer) (*x509.Certificate, error) {
	return NewSignedCert(privateKey, t.CACert[0], t.CAKey, t.CN, t.Organization, domains, ips)
}

func populateCN(secret *v1.Secret, cn ...string) *v1.Secret {
	secret = secret.DeepCopy()
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	for _, cn := range cn {
		if cnRegexp.MatchString(cn) {
			secret.Annotations[getAnnotationKey(cn)] = cn
		} else {
			logrus.Errorf("dropping invalid CN: %s", cn)
		}
	}
	return secret
}

// IsStatic returns true if the Secret has an attribute indicating that it contains
// a static (aka user-provided) certificate, which should not be modified.
func IsStatic(secret *v1.Secret) bool {
	if secret != nil && secret.Annotations != nil {
		return secret.Annotations[Static] == "true"
	}
	return false
}

// NeedsUpdate returns true if any of the CNs are not currently present on the
// secret's Certificate, as recorded in the cnPrefix annotations. It will return
// false if all requested CNs are already present, or if maxSANs is non-zero and has
// been exceeded.
func NeedsUpdate(maxSANs int, secret *v1.Secret, cn ...string) bool {
	if secret == nil {
		return true
	}

	for _, cn := range cn {
		if secret.Annotations[getAnnotationKey(cn)] == "" {
			if maxSANs > 0 && len(cns(secret)) >= maxSANs {
				return false
			}
			return true
		}
	}

	return false
}

func getPrivateKey(secret *v1.Secret) (crypto.Signer, error) {
	keyBytes := secret.Data[v1.TLSPrivateKeyKey]
	if len(keyBytes) == 0 {
		return NewPrivateKey()
	}

	privateKey, err := cert.ParsePrivateKeyPEM(keyBytes)
	if signer, ok := privateKey.(crypto.Signer); ok && err == nil {
		return signer, nil
	}

	return NewPrivateKey()
}

// MarshalChain returns given key and certificates as byte slices.
func MarshalChain(privateKey crypto.Signer, certs ...*x509.Certificate) (keyBytes, certChainBytes []byte, err error) {
	keyBytes, err = cert.MarshalPrivateKeyToPEM(privateKey)
	if err != nil {
		return nil, nil, err
	}

	for _, cert := range certs {
		if cert != nil {
			certBlock := pem.Block{
				Type:  CertificateBlockType,
				Bytes: cert.Raw,
			}
			certChainBytes = append(certChainBytes, pem.EncodeToMemory(&certBlock)...)
		}
	}
	return keyBytes, certChainBytes, nil
}

// Marshal returns the given cert and key as byte slices.
func Marshal(x509Cert *x509.Certificate, privateKey crypto.Signer) (certBytes, keyBytes []byte, err error) {
	certBlock := pem.Block{
		Type:  CertificateBlockType,
		Bytes: x509Cert.Raw,
	}

	keyBytes, err = cert.MarshalPrivateKeyToPEM(privateKey)
	if err != nil {
		return nil, nil, err
	}

	return pem.EncodeToMemory(&certBlock), keyBytes, nil
}

// NewPrivateKey returnes a new ECDSA key
func NewPrivateKey() (crypto.Signer, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// getAnnotationKey return the key to use for a given CN. IPv4 addresses and short hostnames
// are safe to store as-is, but longer hostnames and IPv6 address must be truncated and/or undergo
// character replacement in order to be used as an annotation key. If the CN requires modification,
// a portion of the SHA256 sum of the original value is used as the suffix, to reduce the likelihood
// of collisions in modified values.
func getAnnotationKey(cn string) string {
	cn = cnPrefix + cn
	cnLen := len(cn)
	if cnLen < 64 && !strings.ContainsRune(cn, ':') {
		return cn
	}
	digest := sha256.Sum256([]byte(cn))
	cn = strings.ReplaceAll(cn, ":", "_")
	if cnLen > 56 {
		cnLen = 56
	}
	return cn[0:cnLen] + "-" + hex.EncodeToString(digest[0:])[0:6]
}
