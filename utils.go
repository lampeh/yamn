package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Masterminds/log-go"
	"github.com/crooks/yamn/crandom"
	"github.com/crooks/yamn/linebreaker"
	"github.com/dchest/blake2s"
	//"github.com/codahale/blake2"
)

// min returns the lower of two integers
func min(x, y int) int {
	if x < y {
		return x
	}
	return y
}

// max returns the higher of two integers
func max(x, y int) int {
	if x > y {
		return x
	}
	return y
}

// daysAgo takes a timestamp and returns an integer of its age in days.
func daysAgo(date time.Time) (days int) {
	age := time.Since(date)
	days = int(age.Hours() / 24)
	return
}

// IsMemberStr tests for the membership of a string in a slice
func IsMemberStr(s string, slice []string) bool {
	for _, n := range slice {
		if n == s {
			return true
		}
	}
	return false
}

// readdir returns a list of files in a specified directory that begin with
// the specified prefix.
func readDir(path, prefix string) (files []string, err error) {
	fi, err := ioutil.ReadDir(path)
	if err != nil {
		return
	}
	for _, f := range fi {
		if !f.IsDir() && strings.HasPrefix(f.Name(), prefix) {
			files = append(files, f.Name())
		}
	}
	return
}

// messageID returns an RFC compliant Message-ID for use in message
// construction.
func messageID() (datestr string) {
	dateComponent := time.Now().Format("20060102.150405")
	randomComponent := hex.EncodeToString(crandom.Randbytes(4))
	var domainComponent string
	if cfg.Mail.MessageDomain != "" {
		domainComponent = cfg.Mail.MessageDomain
	} else if strings.Contains(cfg.Remailer.Address, "@") {
		domainComponent = strings.SplitN(
			cfg.Remailer.Address, "@", 2,
		)[1]
	} else {
		domainComponent = "yamn.invalid"
	}
	datestr = fmt.Sprintf(
		"<%s.%s@%s>",
		dateComponent,
		randomComponent,
		domainComponent,
	)
	return
}

// lenCheck verifies that a slice is of a specified length
func lenCheck(got, expected int) (err error) {
	if got != expected {
		err = fmt.Errorf("incorrect length: Expected=%d, Got=%d", expected, got)
		log.Info(err)
	}
	return
}

// bufLenCheck verifies that a given buffer length is of a specified length
func bufLenCheck(buflen, length int) (err error) {
	if buflen != length {
		err = fmt.Errorf("incorrect buffer length: Wanted=%d, Got=%d", length, buflen)
		log.Info(err)
	}
	return
}

// Return the time when filename was last modified
func fileTime(filename string) (t time.Time, err error) {
	info, err := os.Stat(filename)
	if err != nil {
		return
	}
	t = info.ModTime()
	return
}

// httpGet retrieves url and stores it in filename
func httpGet(url, filename string) (err error) {
	res, err := http.Get(url)
	if err != nil {
		return
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		err = fmt.Errorf("%s: %s", url, res.Status)
		return err
	}
	content, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return
	}
	err = ioutil.WriteFile(filename, content, 0644)
	if err != nil {
		return
	}
	return
}

// isPath returns True if a given file or directory exists
func isPath(path string) (bool, error) {
	var err error
	_, err = os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// assertExists panics if a given file or dir doesn't exist
func assertIsPath(path string) {
	testPath, err := isPath(path)
	if err != nil {
		// Some error occurred other than the path not existing
		panic(err)
	}
	if !testPath {
		// Arghh, the path doesn't exist!
		err = fmt.Errorf("assertion failure: path %s does not exist", path)
		panic(err)
	}
}

// sPopBytes returns n bytes from the start of a slice
func sPopBytes(sp *[]byte, n int) (pop []byte, err error) {
	s := *sp
	if len(s) < n {
		err = fmt.Errorf("cannot pop %d bytes from slice of %d", n, len(s))
		return
	}
	pop = s[:n]
	s = s[n:]
	*sp = s
	return
}

// ePopBytes returns n bytes from the end of a slice
func ePopBytes(sp *[]byte, n int) (pop []byte, err error) {
	s := *sp
	if len(s) < n {
		err = fmt.Errorf("cannot pop %d bytes from slice of %d", n, len(s))
		return
	}
	pop = s[len(s)-n:]
	s = s[:len(s)-n]
	*sp = s
	return
}

// popstr takes a pointer to a string slice and pops the last element
func popstr(s *[]string) (element string) {
	slice := *s
	element, slice = slice[len(slice)-1], slice[:len(slice)-1]
	*s = slice
	return
}

// writeInternalHeader inserts a Yamn internal header containing the pooled
// date.  This is useful for performing expiry on old messages.
func writeInternalHeader(w io.Writer) {
	dateHeader := fmt.Sprintf(
		"Yamn-Pooled-Date: %s\n",
		time.Now().Format(shortdate),
	)
	w.Write([]byte(dateHeader))
}

func writeMailHeaders(w io.Writer, sendTo string) {
	w.Write([]byte(fmt.Sprintf("To: %s\n", sendTo)))
	w.Write([]byte(fmt.Sprintf("From: %s\n", cfg.Remailer.Address)))
	w.Write([]byte(fmt.Sprintf("Subject: yamn-%s\n", version)))
	w.Write([]byte("\n"))
}

// wrap64 writes a byte payload as wrapped base64 to an io.writer
func wrap64(writer io.Writer, b []byte, wrap int) {
	breaker := linebreaker.NewLineBreaker(writer, wrap)
	b64 := base64.NewEncoder(base64.StdEncoding, breaker)
	b64.Write(b)
	b64.Close()
	breaker.Close()
}

// armor converts a plain-byte Yamn message to a Base64 armored message with
// cutmarks and header fields.
func armor(w io.Writer, payload []byte) {
	err := lenCheck(len(payload), messageBytes)
	if err != nil {
		panic(err)
	}
	w.Write([]byte("::\n"))
	w.Write([]byte(fmt.Sprintf("Remailer-Type: yamn-%s\n\n", version)))
	w.Write([]byte("-----BEGIN REMAILER MESSAGE-----\n"))
	// Write message length
	w.Write([]byte(strconv.Itoa(len(payload)) + "\n"))
	//digest := blake2.New(&blake2.Config{Size: 16})
	digest, err := blake2s.New(nil)
	if err != nil {
		panic(err)
	}
	digest.Write(payload)
	// Write message digest
	w.Write([]byte(hex.EncodeToString(digest.Sum(nil)) + "\n"))
	// Write the payload to the base64 wrapper
	wrap64(w, payload, base64LineWrap)
	w.Write([]byte("\n-----END REMAILER MESSAGE-----\n"))
}

// stripArmor takes a Mixmaster formatted message from an ioreader and
// returns its payload as a byte slice
func stripArmor(reader io.Reader) (payload []byte, err error) {
	scanner := bufio.NewScanner(reader)
	scanPhase := 0
	b64 := new(bytes.Buffer)
	var statedLen int
	var payloadDigest []byte
	/* Scan phases are:
	0	Expecting ::
	1 Expecting Begin cutmarks
	2 Expecting size
	3	Expecting hash
	4 In payload and checking for End cutmark
	5 Got End cutmark
	*/
	for scanner.Scan() {
		line := scanner.Text()
		switch scanPhase {
		case 0:
			// Expecting ::\n
			if line == "::" {
				scanPhase = 1
				continue
			}
		case 1:
			// Expecting Begin cutmarks
			if line == "-----BEGIN REMAILER MESSAGE-----" {
				scanPhase = 2
			}
		case 2:
			// Expecting size
			statedLen, err = strconv.Atoi(line)
			if err != nil {
				err = fmt.Errorf("unable to extract payload size from %s", line)
				return
			}
			scanPhase = 3
		case 3:
			if len(line) != 64 {
				err = fmt.Errorf("expected 64 digit Hex encoded Hash, got %d bytes", len(line))
				return
			}
			payloadDigest, err = hex.DecodeString(line)
			if err != nil {
				err = errors.New("unable to decode Hex hash on payload")
				return
			}
			scanPhase = 4
		case 4:
			if line == "-----END REMAILER MESSAGE-----" {
				scanPhase = 5
				break
			}
			b64.WriteString(line)
		} // End of switch
	} // End of file scan
	switch scanPhase {
	case 0:
		err = errors.New("no :: found on message")
		return
	case 1:
		err = errors.New("no Begin cutmarks found on message")
		return
	case 4:
		err = errors.New("no End cutmarks found on message")
		return
	}
	payload = make([]byte, base64.StdEncoding.DecodedLen(b64.Len()))
	payloadLen, err := base64.StdEncoding.Decode(payload, b64.Bytes())
	if err != nil {
		return
	}
	// Tuncate payload to the number of decoded bytes
	payload = payload[0:payloadLen]
	// Validate payload length against stated length.
	if statedLen != payloadLen {
		err = fmt.Errorf("payload size doesn't match stated size. Stated=%d, Got=%d", statedLen, payloadLen)
		return
	}
	// Validate payload length against packet format.
	if payloadLen != messageBytes {
		err = fmt.Errorf("payload size doesn't match stated size. Wanted=%d, Got=%d", messageBytes, payloadLen)
		return
	}
	//digest := blake2.New(&blake2.Config{Size: 16})
	digest, err := blake2s.New(nil)
	if err != nil {
		panic(err)
	}
	digest.Write(payload)
	if !bytes.Equal(digest.Sum(nil), payloadDigest) {
		err = errors.New("incorrect payload digest during dearmor")
		return
	}
	return
}
