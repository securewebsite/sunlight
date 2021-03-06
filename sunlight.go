package sunlight

import (
	"bytes"
	"golang.org/x/net/idna"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	_ "github.com/mattn/go-sqlite3"
	"github.com/monicachew/alexa"
	"io/ioutil"
	"net"
	"os"
	"strings"
	"time"
)

const (
	VALID_PERIOD_TOO_LONG          = "ValidPeriodTooLong"
	DEPRECATED_SIGNATURE_ALGORITHM = "DeprecatedSignatureAlgorithm"
	DEPRECATED_VERSION             = "DeprecatedVersion"
	MISSING_CN_IN_SAN              = "MissingCNInSan"
	KEY_TOO_SHORT                  = "KeyTooShort"
	EXP_TOO_SMALL                  = "ExpTooSmall"
)

// Only fields that start with capital letters are exported
type CertSummary struct {
	CN                 string
	Issuer             string
	Sha256Fingerprint  string
	NotBefore          string
	NotAfter           string
	KeySize            int
	Exp                int
	SignatureAlgorithm int
	Version            int
	IsCA               bool
	DnsNames           []string
	IpAddresses        []string
	Violations         map[string]bool
	MaxReputation      float32
	IssuerInMozillaDB  bool
	Timestamp          uint64
}

type IssuerReputationScore struct {
	NormalizedScore float32
	RawScore        float32
}

type IssuerReputation struct {
	Issuer            string
	IssuerInMozillaDB bool
	Scores            map[string]*IssuerReputationScore
	IsCA              uint64
	// Issuer reputation, between [0, 1]. This is only affected by certs that
	// have MaxReputation != -1
	NormalizedScore float32
	// Issuer reputation, between [0, 1]. This is affected by all certs, whether
	// or not they are associated with domains that appear in Alexa.
	RawScore float32
	// Total count of certs issued by this issuer for domains in Alexa.
	NormalizedCount uint64
	// Total count of certs issued by this issuer
	RawCount  uint64
	BeginTime uint64
	done      bool
}

// Given a time since the epoch in milliseconds, returns a time since the
// epoch in milliseconds that is the GMT time of the month that most
// recently began before that time.
func TruncateMonth(t uint64) uint64 {
	// t is in milliseconds, but time.Unix wants its first argument in seconds
	d := time.Unix(int64(t)/1000, 0)
	truncated := time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, time.UTC)
	// again, time.Unix returns seconds - we want milliseconds
	return uint64(truncated.Unix()) * 1000
}

func TimeToJSONString(t time.Time) string {
	const layout = "Jan 2 2006"
	return t.Format(layout)
}

func (summary *CertSummary) ViolatesBR() bool {
	for _, val := range summary.Violations {
		if val {
			return true
		}
	}
	return false
}

func maybeAppendFieldToBuffer(buffer *bytes.Buffer, field []string, prefix string) {
	if len(field) > 0 && len(field[0]) > 0 {
		if buffer.Len() > 0 {
			fmt.Fprint(buffer, ", ")
		}
		fmt.Fprint(buffer, prefix, field[0])
	}
}

func DistinguishedNameToString(n pkix.Name) string {
	buffer := bytes.NewBufferString("")
	// This is strange: x509.pkix.Name is defined as:
	// type Name struct {
	//   Country, Organization, OrganizationalUnit []string
	//   Locality, Province                        []string
	//   StreetAddress, PostalCode                 []string
	//   SerialNumber, CommonName                  string
	//
	//   Names []AttributeTypeAndValue
	// }
	// so in theory there could be multiple values for Country, Organization, etc.
	// (except for SerialNumber and CommonName, the former of which we're completely
	// ignoring anyway). We'll just be lazy and take the first of each.
	// Also, since our list of root CAs only uses Organization, OrganizationalUnit,
	// and CommonName, we'll only consider those.
	maybeAppendFieldToBuffer(buffer, n.Organization, "O=")
	maybeAppendFieldToBuffer(buffer, n.OrganizationalUnit, "OU=")
	maybeAppendFieldToBuffer(buffer, []string{n.CommonName}, "CN=")
	return buffer.String()
}

func containsIssuerInRootList(certChain []*x509.Certificate, rootCAMap map[string]bool) bool {
	for _, cert := range certChain {
		if rootCAMap[DistinguishedNameToString(cert.Issuer)] {
			return true
		}
	}
	return false
}

func NewIssuerReputation(issuer pkix.Name, timestamp uint64) *IssuerReputation {
	reputation := new(IssuerReputation)
	reputation.BeginTime = TruncateMonth(timestamp)
	reputation.Issuer = DistinguishedNameToString(issuer)
	reputation.Scores = make(map[string]*IssuerReputationScore)
	return reputation
}

func (score *IssuerReputationScore) Update(reputation float32) {
	score.NormalizedScore += reputation
	score.RawScore += 1
}

func (score *IssuerReputationScore) Finish(normalizedCount uint64,
	rawCount uint64) {
	score.NormalizedScore /= float32(normalizedCount)
	// We want low scores to be bad and high scores to be good, similar to Alexa
	score.NormalizedScore = 1.0 - score.NormalizedScore
	score.RawScore /= float32(rawCount)
	score.RawScore = 1.0 - score.RawScore
}

func (issuer *IssuerReputation) Update(summary *CertSummary) {
	issuer.RawCount += 1
	issuer.IssuerInMozillaDB = summary.IssuerInMozillaDB
	reputation := summary.MaxReputation
	if reputation != -1 {
		// Keep track of certs issued for domains in Alexa
		issuer.NormalizedCount += 1
	} else {
		reputation = 0
	}

	for name, val := range summary.Violations {
		if issuer.Scores[name] == nil {
			issuer.Scores[name] = new(IssuerReputationScore)
		}
		if val {
			issuer.Scores[name].Update(reputation)
		}
	}

	if summary.IsCA {
		issuer.IsCA += 1
	}
}

func (issuer *IssuerReputation) Finish() {
	normalizedSum := float32(0.0)
	rawSum := float32(0.0)
	for _, score := range issuer.Scores {
		score.Finish(issuer.NormalizedCount, issuer.RawCount)
		normalizedSum += score.NormalizedScore
		rawSum += score.RawScore
	}
	issuer.NormalizedScore = normalizedSum / float32(len(issuer.Scores))
	issuer.RawScore = rawSum / float32(len(issuer.Scores))
}

func CalculateCertSummary(cert *x509.Certificate, timestamp uint64, ranker *alexa.AlexaRank,
	certChain []*x509.Certificate, rootCAMap map[string]bool) (result *CertSummary, err error) {
	summary := CertSummary{}
	summary.Timestamp = timestamp
	summary.CN = cert.Subject.CommonName
	summary.Issuer = DistinguishedNameToString(cert.Issuer)
	summary.NotBefore = TimeToJSONString(cert.NotBefore)
	summary.NotAfter = TimeToJSONString(cert.NotAfter)
	summary.IsCA = cert.IsCA
	summary.Version = cert.Version
	summary.SignatureAlgorithm = int(cert.SignatureAlgorithm)
	summary.Violations = map[string]bool{
		VALID_PERIOD_TOO_LONG:          false,
		DEPRECATED_SIGNATURE_ALGORITHM: false,
		DEPRECATED_VERSION:             cert.Version != 3,
		KEY_TOO_SHORT:                  false,
		EXP_TOO_SMALL:                  false,
		MISSING_CN_IN_SAN:              false,
	}

	// BR 9.4.1: Validity period is longer than 5 years.  This
	// should be restricted to certs that don't have CA:True
	if cert.NotAfter.After(cert.NotBefore.AddDate(5, 0, 7)) &&
		(!cert.BasicConstraintsValid ||
			(cert.BasicConstraintsValid && !cert.IsCA)) {
		summary.Violations[VALID_PERIOD_TOO_LONG] = true
	}

	// SignatureAlgorithm is SHA1
	if cert.SignatureAlgorithm == x509.SHA1WithRSA ||
		cert.SignatureAlgorithm == x509.DSAWithSHA1 ||
		cert.SignatureAlgorithm == x509.ECDSAWithSHA1 {
		summary.Violations[DEPRECATED_SIGNATURE_ALGORITHM] = true
	}

	// Public key length <= 1024 bits
	summary.KeySize = -1
	summary.Exp = -1
	parsedKey, ok := cert.PublicKey.(*rsa.PublicKey)
	if ok {
		summary.KeySize = parsedKey.N.BitLen()
		summary.Exp = parsedKey.E
		if summary.KeySize <= 1024 {
			summary.Violations[KEY_TOO_SHORT] = true
		}
		if summary.Exp <= 3 {
			summary.Violations[EXP_TOO_SMALL] = true
		}
	}

	if ranker != nil {
		summary.MaxReputation, _ = ranker.GetReputation(cert.Subject.CommonName)
		for _, host := range cert.DNSNames {
			reputation, _ := ranker.GetReputation(host)
			if reputation > summary.MaxReputation {
				summary.MaxReputation = reputation
			}
		}
	}
	sha256hasher := sha256.New()
	sha256hasher.Write(cert.Raw)
	summary.Sha256Fingerprint = base64.StdEncoding.EncodeToString(sha256hasher.Sum(nil))

	// DNS names and IP addresses
	summary.DnsNames = cert.DNSNames
	for _, address := range cert.IPAddresses {
		summary.IpAddresses = append(summary.IpAddresses, address.String())
	}

	summary.IssuerInMozillaDB = containsIssuerInRootList(certChain, rootCAMap)

	// Assume a 0-length CN means it isn't present (this isn't a good
	// assumption). If the CN is missing, then it can't be missing CN in SAN.
	if len(cert.Subject.CommonName) == 0 {
		return &summary, nil
	}

	cnAsPunycode, err := idna.ToASCII(cert.Subject.CommonName)
	if err != nil {
		return &summary, nil
	}

	// BR 9.2.2: Found Common Name in Subject Alt Names, either as an IP or a
	// DNS name.
	summary.Violations[MISSING_CN_IN_SAN] = true
	cnAsIP := net.ParseIP(cert.Subject.CommonName)
	if cnAsIP != nil {
		for _, ip := range cert.IPAddresses {
			if cnAsIP.Equal(ip) {
				summary.Violations[MISSING_CN_IN_SAN] = false
			}
		}
	} else {
		for _, san := range cert.DNSNames {
			if err == nil && strings.EqualFold(san, cnAsPunycode) {
				summary.Violations[MISSING_CN_IN_SAN] = false
			}
		}
	}
	return &summary, nil
}

// Takes the name of a file containing newline-delimited Subject Names (as
// interpreted by DistinguishedNameToString) that each correspond to a
// certificate in Mozilla's root CA program. Returns these names as a map of
// string -> bool.
func ReadRootCAMap(filename string) map[string]bool {
	caStringBytes, err := ioutil.ReadFile(filename)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open root CA list at %s: %s\n",
			filename, err)
		os.Exit(1)
	}
	rootCAMap := make(map[string]bool)
	for _, ca := range strings.Split(string(caStringBytes), "\n") {
		rootCAMap[ca] = true
	}
	return rootCAMap
}
