package main

import (
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	_ "github.com/mattn/go-sqlite3"
	"github.com/monicachew/alexa"
	"github.com/monicachew/certificatetransparency"
	. "github.com/mozkeeler/sunlight"
	"os"
	"regexp"
	"runtime"
	"sync"
	"time"
)

// Flags
var alexaFile string
var dbFile string
var ctLog string
var jsonFile string
var maxEntries uint64
var rootCAFile string

func init() {
	flag.StringVar(&alexaFile, "alexa_file", "top-1m.csv",
		"CSV containing <rank, domain>")
	flag.StringVar(&dbFile, "db_file", "BRs.db", "File for creating sqlite DB")
	flag.StringVar(&ctLog, "ct_log", "ct_entries.log", "File containing CT log")
	flag.StringVar(&jsonFile, "json_file", "certs.json", "JSON summary output")
	flag.Uint64Var(&maxEntries, "max_entries", 0, "Max entries (0 means all)")
	flag.StringVar(&rootCAFile, "rootCA_file", "rootCAList.txt", "list of root CA CNs")
	runtime.GOMAXPROCS(runtime.NumCPU())
}

func certToString(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}
	b64 := base64.StdEncoding.EncodeToString(cert.Raw)
	re := regexp.MustCompile(`(\S{64})`)
	b64WithNewlines := re.ReplaceAllString(b64, "$1\r\n")
	return "-----BEGIN CERTIFICATE-----\r\n" + b64WithNewlines + "\r\n-----END CERTIFICATE-----\r\n"
}

func main() {
	flag.Parse()
	if flag.NArg() != 0 {
		flag.PrintDefaults()
		os.Exit(1)
	}

	var ranker alexa.AlexaRank
	ranker.Init(alexaFile)
	db, err := sql.Open("sqlite3", dbFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open %s: %s\n", dbFile, err)
		flag.PrintDefaults()
		os.Exit(1)
	}
	defer db.Close()

	createTables := `
	drop table if exists baselineRequirements;
	create table baselineRequirements(
		cn text, issuer text,
		sha256Fingerprint text, notBefore date,
		notAfter date, validPeriodTooLong bool,
		deprecatedSignatureAlgorithm bool,
		deprecatedVersion bool,
		missingCNinSAN bool, keyTooShort bool,
		keySize integer, expTooSmall bool,
		exp integer, signatureAlgorithm integer,
		version integer, dnsNames string,
		ipAddresses string, maxReputation float,
		issuerInMozillaDB bool,
		timestamp bigint);
	drop table if exists issuerReputation;
	create table issuerReputation(
		issuer text,
		issuerInMozillaDB bool,
		validPeriodTooLongNormalizedScore float,
		validPeriodTooLongRawScore float,
		deprecatedVersionNormalizedScore float,
		deprecatedVersionRawScore float,
		deprecatedSignatureAlgorithmNormalizedScore float,
		deprecatedSignatureAlgorithmRawScore float,
		missingCNinSANNormalizedScore float,
		missingCNinSANRawScore float,
		keyTooShortNormalizedScore float,
		keyTooShortRawScore float,
		expTooSmallNormalizedScore float,
		expTooSmallRawScore float,
		normalizedScore float,
		rawScore float,
		normalizedCount integer,
		rawCount integer,
		beginTime bigint);
	drop table if exists examples;
	create table examples(
		issuer text,
		validPeriodTooLongExample text,
		validPeriodTooLongLastSeen bigint,
		deprecatedVersionExample text,
		deprecatedVersionLastSeen bigint,
		deprecatedSignatureAlgorithmExample text,
		deprecatedSignatureAlgorithmLastSeen bigint,
		missingCNinSANExample text,
		missingCNinSANLastSeen bigint,
		keyTooShortExample text,
		keyTooShortLastSeen bigint,
		expTooSmallExample text,
		expTooSmallLastSeen bigint);
	`

	_, err = db.Exec(createTables)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create table: %s\n", err)
		os.Exit(1)
	}

	tx, err := db.Begin()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to begin using DB: %s\n", err)
		os.Exit(1)
	}

	insertEntry := `
	insert into baselineRequirements(
		cn, issuer, sha256Fingerprint, notBefore,
		notAfter, validPeriodTooLong,
		deprecatedSignatureAlgorithm,
		deprecatedVersion, missingCNinSAN,
		keyTooShort, keySize, expTooSmall, exp,
		signatureAlgorithm, version, dnsNames,
		ipAddresses, maxReputation,
		issuerInMozillaDB, timestamp)
		values(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	insertEntryStatement, err := tx.Prepare(insertEntry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create prepared statement: %s\n", err)
		os.Exit(1)
	}
	defer insertEntryStatement.Close()

	insertIssuer := `
	 insert into issuerReputation(
		issuer,
		issuerInMozillaDB,
		validPeriodTooLongNormalizedScore, validPeriodTooLongRawScore,
		deprecatedVersionNormalizedScore, deprecatedVersionRawScore,
		deprecatedSignatureAlgorithmNormalizedScore,
		deprecatedSignatureAlgorithmRawScore,
		missingCNinSANNormalizedScore, missingCNinSANRawScore,
		keyTooShortNormalizedScore, keyTooShortRawScore,
		expTooSmallNormalizedScore, expTooSmallRawScore,
		normalizedScore, rawScore,
		normalizedCount, rawCount, beginTime)
	values(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	insertIssuerStatement, err := tx.Prepare(insertIssuer)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create prepared statement: %s\n", err)
		os.Exit(1)
	}
	defer insertIssuerStatement.Close()

	insertExample := `
		insert into examples(
			issuer,
			validPeriodTooLongExample,
			validPeriodTooLongLastSeen,
			deprecatedVersionExample,
			deprecatedVersionLastSeen,
			deprecatedSignatureAlgorithmExample,
			deprecatedSignatureAlgorithmLastSeen,
			missingCNinSANExample,
			missingCNinSANLastSeen,
			keyTooShortExample,
			keyTooShortLastSeen,
			expTooSmallExample,
			expTooSmallLastSeen)
		values(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	insertExampleStatement, err := tx.Prepare(insertExample)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create prepared statement: %s\n", err)
		os.Exit(1)
	}
	defer insertExampleStatement.Close()

	fmt.Fprintf(os.Stderr, "Starting %s\n", time.Now())
	in, err := os.Open(ctLog)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open entries file: %s\n", err)
		flag.PrintDefaults()
		os.Exit(1)
	}
	defer in.Close()

	entriesFile := certificatetransparency.EntriesFile{in}
	fmt.Fprintf(os.Stderr, "Initialized entries %s\n", time.Now())
	out, err := os.OpenFile(jsonFile, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open JSON output file %s: %s\n",
			jsonFile, err)
		flag.PrintDefaults()
	}

	fmt.Fprintf(out, "{\"Certs\":[")
	firstOutLock := new(sync.Mutex)
	firstOut := true

	rootCAMap := ReadRootCAMap(rootCAFile)

	issuersLock := new(sync.Mutex)
	issuers := make(map[string]*IssuerReputation)

	exampleMapLock := new(sync.Mutex)
	exampleMap := make(map[string]map[string]*x509.Certificate)
	exampleMapLastSeen := make(map[string]map[string]uint64)

	entriesFile.Map(func(ent *certificatetransparency.EntryAndPosition, err error) {
		if err != nil {
			return
		}

		cert, err := x509.ParseCertificate(ent.Entry.X509Cert)
		if err != nil {
			return
		}

		// Filter out certs issued before 2013 or that have already
		// expired.
		now := time.Now()
		if cert.NotBefore.Before(time.Date(2013, 1, 1, 0, 0, 0, 0, time.UTC)) ||
			cert.NotAfter.Before(now) {
			return
		}

		certList := make([]*x509.Certificate, 0)
		for _, certBytes := range ent.Entry.ExtraCerts {
			nextCert, err := x509.ParseCertificate(certBytes)
			if err != nil {
				continue
			}
			certList = append(certList, nextCert)
		}

		summary, err := CalculateCertSummary(cert, ent.Entry.Timestamp, &ranker, certList, rootCAMap)
		if err != nil {
			return
		}
		if summary == nil {
			fmt.Fprintf(os.Stderr, "Couldn't allocate new cert summary\n")
			os.Exit(1)
		}
		certIssuerDN := DistinguishedNameToString(cert.Issuer)
		key := fmt.Sprintf("%s:%d", certIssuerDN, TruncateMonth(ent.Entry.Timestamp))
		issuersLock.Lock()
		if issuers[key] == nil {
			issuers[key] = NewIssuerReputation(cert.Issuer, ent.Entry.Timestamp)
		}
		if issuers[key] == nil {
			fmt.Fprintf(os.Stderr, "Couldn't allocate new issuer reputation\n")
			os.Exit(1)
		}
		// Update issuer reputation whether or not the cert violates baseline
		// requirements.
		issuers[key].Update(summary)
		issuersLock.Unlock()
		if summary.ViolatesBR() {
			dnsNamesAsString, err := json.Marshal(summary.DnsNames)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Failed to convert to JSON: %s\n", err)
				os.Exit(1)
			}
			ipAddressesAsString, err := json.Marshal(summary.IpAddresses)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Failed to convert to JSON: %s\n", err)
				os.Exit(1)
			}
			_, err = insertEntryStatement.Exec(summary.CN, summary.Issuer,
				summary.Sha256Fingerprint,
				cert.NotBefore, cert.NotAfter,
				summary.Violations[VALID_PERIOD_TOO_LONG],
				summary.Violations[DEPRECATED_SIGNATURE_ALGORITHM],
				summary.Violations[DEPRECATED_VERSION],
				summary.Violations[MISSING_CN_IN_SAN],
				summary.Violations[KEY_TOO_SHORT], summary.KeySize,
				summary.Violations[EXP_TOO_SMALL], summary.Exp,
				summary.SignatureAlgorithm,
				summary.Version, dnsNamesAsString,
				ipAddressesAsString,
				summary.MaxReputation,
				summary.IssuerInMozillaDB,
				summary.Timestamp)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Failed to insert entry: %s\n", err)
				os.Exit(1)
			}
			marshalled, err := json.Marshal(summary)
			if err == nil {
				separator := ",\n"
				firstOutLock.Lock()
				if firstOut {
					separator = "\n"
				}
				fmt.Fprintf(out, "%s", separator)
				out.Write(marshalled)
				firstOut = false
				firstOutLock.Unlock()
			} else {
				fmt.Fprintf(os.Stderr, "Couldn't write json: %s\n", err)
				os.Exit(1)
			}

			exampleMapLock.Lock()
			if exampleMap[certIssuerDN] == nil {
				exampleMap[certIssuerDN] = make(map[string]*x509.Certificate)
				exampleMapLastSeen[certIssuerDN] = make(map[string]uint64)
			}
			for violation, isViolation := range summary.Violations {
				if isViolation {
					exampleMap[certIssuerDN][violation] = cert
					exampleMapLastSeen[certIssuerDN][violation] = ent.Entry.Timestamp
				}
			}
			exampleMapLock.Unlock()
		}
	}, maxEntries)
	fmt.Fprintf(out, "]}\n")
	// Normalize all our scores
	for _, issuer := range issuers {
		issuer.Finish()
		_, err = insertIssuerStatement.Exec(issuer.Issuer,
			issuer.IssuerInMozillaDB,
			issuer.Scores[VALID_PERIOD_TOO_LONG].NormalizedScore,
			issuer.Scores[VALID_PERIOD_TOO_LONG].RawScore,
			issuer.Scores[DEPRECATED_VERSION].NormalizedScore,
			issuer.Scores[DEPRECATED_VERSION].RawScore,
			issuer.Scores[DEPRECATED_SIGNATURE_ALGORITHM].NormalizedScore,
			issuer.Scores[DEPRECATED_SIGNATURE_ALGORITHM].RawScore,
			issuer.Scores[MISSING_CN_IN_SAN].NormalizedScore,
			issuer.Scores[MISSING_CN_IN_SAN].RawScore,
			issuer.Scores[KEY_TOO_SHORT].NormalizedScore,
			issuer.Scores[KEY_TOO_SHORT].RawScore,
			issuer.Scores[EXP_TOO_SMALL].NormalizedScore,
			issuer.Scores[EXP_TOO_SMALL].RawScore,
			issuer.NormalizedScore,
			issuer.RawScore,
			issuer.NormalizedCount,
			issuer.RawCount,
			issuer.BeginTime)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to insert entry: %s\n", err)
			os.Exit(1)
		}
	}

	for issuer, examples := range exampleMap {
		_, err = insertExampleStatement.Exec(issuer,
			certToString(examples[VALID_PERIOD_TOO_LONG]),
			exampleMapLastSeen[issuer][VALID_PERIOD_TOO_LONG],
			certToString(examples[DEPRECATED_VERSION]),
			exampleMapLastSeen[issuer][DEPRECATED_VERSION],
			certToString(examples[DEPRECATED_SIGNATURE_ALGORITHM]),
			exampleMapLastSeen[issuer][DEPRECATED_SIGNATURE_ALGORITHM],
			certToString(examples[MISSING_CN_IN_SAN]),
			exampleMapLastSeen[issuer][MISSING_CN_IN_SAN],
			certToString(examples[KEY_TOO_SHORT]),
			exampleMapLastSeen[issuer][KEY_TOO_SHORT],
			certToString(examples[EXP_TOO_SMALL]),
			exampleMapLastSeen[issuer][EXP_TOO_SMALL])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to insert entry: %s\n", err)
			os.Exit(1)
		}
	}
	tx.Commit()
}
