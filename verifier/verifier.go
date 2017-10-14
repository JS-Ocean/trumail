package verifier

import (
	"encoding/xml"
	"strings"

	"golang.org/x/sync/errgroup"
)

// Lookup contains all output data for an email validation Lookup
type Lookup struct {
	XMLName      xml.Name `json:"-" xml:"lookup"`
	Address      string   `json:"address,omitempty" xml:"address,omitempty"`
	Username     string   `json:"username,omitempty" xml:"username,omitempty"`
	Domain       string   `json:"domain,omitempty" xml:"domain,omitempty"`
	HostExists   bool     `json:"hostExists" xml:"hostExists"`
	Deliverable  bool     `json:"deliverable" xml:"deliverable"`
	FullInbox    bool     `json:"fullInbox" xml:"fullInbox"`
	CatchAll     bool     `json:"catchAll" xml:"catchAll"`
	Disposable   bool     `json:"disposable" xml:"disposable"`
	Gravatar     bool     `json:"gravatar" xml:"gravatar"`
	Error        string   `json:"error,omitempty" xml:"error,omitempty"`
	ErrorDetails string   `json:"errorDetails,omitempty" xml:"errorDetails,omitempty"`
}

// Verifier defines all functionality for fully validating email addresses
type Verifier interface {
	Verify(emails ...string) []*Lookup
}

// verifier contains all data needed to perform educated email verification
// lookups
type verifier struct {
	maxWorkerCount int    // Maximum number of concurrent domain validation workers
	hostname       string // This machines hostname
	sourceAddr     string // The source email address
	disposabler    Disposabler
}

// NewVerifier generates a new AddressVerifier reference
func NewVerifier(maxWorkerCount int, hostname, sourceAddr string) Verifier {
	return &verifier{
		maxWorkerCount: maxWorkerCount,
		hostname:       hostname,
		sourceAddr:     sourceAddr,
		disposabler:    NewDisposabler(),
	}
}

// Verify performs all threaded operations involved with validating
// one or more email addresses
func (v *verifier) Verify(emails ...string) []*Lookup {
	var totalLookups int
	var lookups []*Lookup

	// Organize all the addresses into a map of domain - address, address...
	domainQueue := make(map[string][]*Address)
	for _, email := range emails {
		address, err := ParseAddress(email)
		if err != nil {
			lookups = append(lookups, &Lookup{
				Error: "Failed to parse email",
			})
			continue
		}
		domainQueue[address.Domain] = append(domainQueue[address.Domain], address)
		totalLookups++
	}

	// Don't create channels or workers if there's no work to do
	if len(domainQueue) == 0 {
		return lookups
	}

	// Makes two channels that hold both a queue of Addresses and results
	// of all validations that take place
	jobs := make(chan []*Address, len(domainQueue))
	results := make(chan *Lookup, totalLookups)

	// Generate NO MORE than v.maxWorkerCount workers
	workers := v.maxWorkerCount
	if len(domainQueue) < workers {
		workers = len(domainQueue)
	}

	// For as long as workers specifies, generate a goroutine to Verify every
	// address on the same connection
	for w := 1; w <= workers; w++ {
		go v.worker(jobs, results)
	}

	// Dump a collection of jobs for each domain onto the jobs channel
	for _, addresses := range domainQueue {
		jobs <- addresses
	}
	close(jobs)

	// Pull all the results out of the Lookup results channel and returns
	for w := 1; w <= len(domainQueue); w++ {
		lookups = append(lookups, <-results)
	}
	return lookups
}

// worker receives a domain, an array of addresses and a channel where
// we can place the validation results. Workers are generated for each domain
// and the deliverabler connection is closed once finished
func (v *verifier) worker(jobs <-chan []*Address, results chan<- *Lookup) {
	for j := range jobs {
		var deliverabler Deliverabler
		// Defines the domain specific constant variables
		var disposable, catchAll bool
		var basicErr, detailErr string

		// Attempts to form an SMTP Connection and returns either a Deliverabler
		// or an error which will be parsed and returned in the lookup
		deliverabler, err := NewDeliverabler(j[0].Domain, v.hostname, v.sourceAddr)
		if err != nil {
			basicErr, detailErr = parseErr(err)
		}

		// Retrieves the catchall status if there's a deliverabler and we don't yet
		// have any catchall status
		if deliverabler != nil {
			if deliverabler.HasCatchAll(j[0].Domain, 5) {
				catchAll = true
			}
		}
		disposable = v.disposabler.IsDisposable(j[0].Domain)

		// Builds a validation for every email defined for the domain
		for _, address := range j {
			// Performs address specific validation
			var deliverable, fullInbox, gravatar bool
			var g errgroup.Group

			// Concurrently retrieve final validation info
			g.Go(func() error {
				if catchAll {
					deliverable = true // Catchall domains will always be deliverable
				} else if deliverabler != nil {
					if err := deliverabler.IsDeliverable(address.Address, 5); err == nil {
						deliverable = true
					} else if err == ErrFullInbox {
						fullInbox = true
					}
				}
				return nil
			})
			g.Go(func() error {
				gravatar = HasGravatar(address)
				return nil
			})
			g.Wait()

			// Add each new validation Lookup to the results channel
			results <- &Lookup{
				Address:      address.Address,
				Username:     address.Username,
				Domain:       address.Domain,
				HostExists:   !strings.Contains(detailErr, "no such host"),
				Deliverable:  deliverable,
				FullInbox:    fullInbox,
				Disposable:   disposable,
				CatchAll:     catchAll,
				Gravatar:     gravatar,
				Error:        basicErr,
				ErrorDetails: detailErr,
			}
		}

		// Close the connection with the MX server now that we are finished
		if deliverabler != nil {
			deliverabler.Close()
		}
	}
}

// parseErr parses an error in order to return a more user friendly version of
// the error
func parseErr(err error) (string, string) {
	if err != nil {
		errStr := err.Error()
		switch {
		case strings.Contains(errStr, "timeout"):
			return "The connection to the mail server has timed out", errStr
		case strings.Contains(errStr, "no such host"):
			return "Mail server does not exist", errStr
		case strings.Contains(errStr, "unavailable"):
			return "Mail server is unavailable", errStr
		case strings.Contains(errStr, "block"):
			return "Blocked by mail server", errStr
		default:
			return errStr, errStr
		}
	}
	return "", ""
}
