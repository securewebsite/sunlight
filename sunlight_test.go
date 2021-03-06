package sunlight

import (
	"bytes"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/json"
	"encoding/pem"
	"testing"
	"time"
)

const pemCertificate = `-----BEGIN CERTIFICATE-----
MIIB5DCCAZCgAwIBAgIBATALBgkqhkiG9w0BAQUwLTEQMA4GA1UEChMHQWNtZSBDbzEZMBcGA1UE
AxMQdGVzdC5leGFtcGxlLmNvbTAeFw03MDAxMDEwMDE2NDBaFw03MDAxMDIwMzQ2NDBaMC0xEDAO
BgNVBAoTB0FjbWUgQ28xGTAXBgNVBAMTEHRlc3QuZXhhbXBsZS5jb20wWjALBgkqhkiG9w0BAQED
SwAwSAJBALKZD0nEffqM1ACuak0bijtqE2QrI/KLADv7l3kK3ppMyCuLKoF0fd7Ai2KW5ToIwzFo
fvJcS/STa6HA5gQenRUCAwEAAaOBnjCBmzAOBgNVHQ8BAf8EBAMCAAQwDwYDVR0TAQH/BAUwAwEB
/zANBgNVHQ4EBgQEAQIDBDAPBgNVHSMECDAGgAQBAgMEMBsGA1UdEQQUMBKCEHRlc3QuZXhhbXBs
ZS5jb20wDwYDVR0gBAgwBjAEBgIqAzAqBgNVHR4EIzAhoB8wDoIMLmV4YW1wbGUuY29tMA2CC2V4
YW1wbGUuY29tMAsGCSqGSIb3DQEBBQNBAHKZKoS1wEQOGhgklx4+/yFYQlnqwKXvar/ZecQvJwui
0seMQnwBhwdBkHfVIU2Fu5VUMRyxlf0ZNaDXcpU581k=
-----END CERTIFICATE-----`

func TestCertSummary(t *testing.T) {
	pemBlock, _ := pem.Decode([]byte(pemCertificate))
	cert, _ := x509.ParseCertificate(pemBlock.Bytes)
	fakeRootCAMap := make(map[string]bool)
	fakeCertList := make([]*x509.Certificate, 0)
	ts := uint64(time.Now().Unix())
	summary, _ := CalculateCertSummary(cert, ts, nil, fakeCertList, fakeRootCAMap)
	expected := CertSummary{
		CN:                 "test.example.com",
		Issuer:             "O=Acme Co, CN=test.example.com",
		Sha256Fingerprint:  "Gvp+Qw6i96YPjUZoO2zqLWdusngA8xpAtvMBouj+MZ8=",
		NotBefore:          "Jan 1 1970",
		NotAfter:           "Jan 2 1970",
		KeySize:            512,
		Exp:                65537,
		SignatureAlgorithm: 3,
		Version:            3,
		IsCA:               true,
		DnsNames:           []string{"test.example.com"},
		IpAddresses:        nil,
		Violations: map[string]bool{
			DEPRECATED_SIGNATURE_ALGORITHM: true,
			DEPRECATED_VERSION:             false,
			EXP_TOO_SMALL:                  false,
			KEY_TOO_SHORT:                  true,
			MISSING_CN_IN_SAN:              false,
			VALID_PERIOD_TOO_LONG:          false,
		},
		MaxReputation: 0,
		Timestamp:     ts,
	}
	b, _ := json.MarshalIndent(summary, "", "  ")
	expected_b, _ := json.MarshalIndent(expected, "", "  ")
	if !bytes.Equal(expected_b, b) {
		t.Errorf("Didn't get expected summary: %b \n!= \n%b\n", expected_b, b)
	}
}

func TestIssuerReputation(t *testing.T) {
	ts := uint64(time.Now().Unix())
	summary := CertSummary{
		CN:                "example.com",
		Issuer:            "CN=Honest Al",
		Sha256Fingerprint: "foo",
		Violations: map[string]bool{
			VALID_PERIOD_TOO_LONG:          true,
			DEPRECATED_SIGNATURE_ALGORITHM: false,
			DEPRECATED_VERSION:             false,
			KEY_TOO_SHORT:                  false,
			EXP_TOO_SMALL:                  false,
			MISSING_CN_IN_SAN:              true,
		},
		MaxReputation:     0.1,
		IssuerInMozillaDB: false,
		Timestamp:         ts,
	}
	unknown_summary := CertSummary{
		CN:                "unknown.example.com",
		Issuer:            "CN=Honest Al",
		Sha256Fingerprint: "foo",
		Violations: map[string]bool{
			VALID_PERIOD_TOO_LONG:          true,
			DEPRECATED_SIGNATURE_ALGORITHM: false,
			DEPRECATED_VERSION:             false,
			KEY_TOO_SHORT:                  false,
			EXP_TOO_SMALL:                  false,
			MISSING_CN_IN_SAN:              true,
		},
		IsCA:              false,
		MaxReputation:     -1,
		IssuerInMozillaDB: false,
		Timestamp:         ts,
	}
	// SEQUENCE of SET of SEQUENCE of OID (CN), PrintableString ("Honest Al")
	subjectBytes := []byte{0x30, 0x14, 0x31, 0x12, 0x30, 0x10, 0x06, 0x03, 0x55, 0x04, 0x03, 0x13, 0x09, 0x48, 0x6f, 0x6e, 0x65, 0x73, 0x74, 0x20, 0x41, 0x6c}
	var subject pkix.RDNSequence
	_, err := asn1.Unmarshal(subjectBytes, &subject)
	if err != nil {
		t.Error("could not decode expected subject RDN", err)
	}
	var name pkix.Name
	name.FillFromRDNSequence(&subject)
	issuer := NewIssuerReputation(name, ts)
	issuer.Update(&summary)
	issuer.Update(&unknown_summary)
	issuer.Finish()
	if issuer.RawCount != 2 {
		t.Error("Should have raw count of 2")
	}
	if issuer.NormalizedCount != 1 {
		t.Error("Should have normalized count of 1")
	}
	if issuer.Scores[VALID_PERIOD_TOO_LONG].NormalizedScore != 0.9 {
		t.Error("Should have score of 0.9")
	}
	if issuer.IssuerInMozillaDB {
		t.Error("Should not be in mozilla db")
	}
	expected_issuer := IssuerReputation{
		Issuer: "CN=Honest Al",
		Scores: map[string]*IssuerReputationScore{
			DEPRECATED_SIGNATURE_ALGORITHM: {
				NormalizedScore: 1,
				RawScore:        1,
			},
			DEPRECATED_VERSION: {
				NormalizedScore: 1,
				RawScore:        1,
			},
			EXP_TOO_SMALL: {
				NormalizedScore: 1,
				RawScore:        1,
			},
			KEY_TOO_SHORT: {
				NormalizedScore: 1,
				RawScore:        1,
			},
			MISSING_CN_IN_SAN: {
				NormalizedScore: 0.9,
				RawScore:        0,
			},
			VALID_PERIOD_TOO_LONG: {
				NormalizedScore: 0.9,
				RawScore:        0,
			},
		},
		IsCA:            0,
		NormalizedScore: 0.9666667,
		RawScore:        0.6666667,
		NormalizedCount: 1,
		RawCount:        2,
		BeginTime:       TruncateMonth(ts),
	}
	b, _ := json.MarshalIndent(issuer, "", "  ")
	expected_b, _ := json.MarshalIndent(expected_issuer, "", "  ")
	if !bytes.Equal(expected_b, b) {
		t.Errorf("Didn't get expected reputation: %s \n!= \n%s\n", expected_b, b)
	}
}
