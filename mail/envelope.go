package mail

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/sloonz/go-qprintable"
	"gopkg.in/iconv.v1"
	"io"
	"io/ioutil"
	"net/textproto"
	"regexp"
	"strings"
	"time"
)

const maxHeaderChunk = iota + (3 << 10) // 3KB

// Address encodes an email address of the form `<user@host>`
type Address struct {
	User string
	Host string
}

func (ep *Address) String() string {
	return fmt.Sprintf("%s@%s", ep.User, ep.Host)
}

func (ep *Address) IsEmpty() bool {
	return ep.User == "" && ep.Host == ""
}

// Email represents a single SMTP message.
type Envelope struct {
	// Remote IP address
	RemoteIP string
	// Message sent in EHLO command
	Helo string
	// Sender
	MailFrom Address
	// Recipients
	RcptTo []Address
	// Data stores the header and message body
	Data bytes.Buffer
	// Subject stores the subject of the email, extracted and decoded after calling ParseHeaders()
	Subject string
	// TLS is true if the email was received using a TLS connection
	TLS bool
	// Header stores the results from ParseHeaders()
	Header textproto.MIMEHeader
	// Values hold the values generated when processing the envelope by the backend
	Values map[string]interface{}
	// Hashes of each email on the rcpt
	Hashes []string
	// additional delivery header that may be added
	DeliveryHeader string
	// Email(s) will be queued with this id
	QueuedId string
}

func NewEnvelope(remoteAddr string, clientID uint64) *Envelope {
	return &Envelope{
		RemoteIP: remoteAddr,
		Values:   make(map[string]interface{}),
		QueuedId: queuedID(clientID),
	}
}

func queuedID(clientID uint64) string {
	return fmt.Sprintf("%x", md5.Sum([]byte(string(time.Now().Unix())+string(clientID))))
}

// ParseHeaders parses the headers into Header field of the Envelope struct.
// Data buffer must be full before calling.
// It assumes that at most 30kb of email data can be a header
// Decoding of encoding to UTF is only done on the Subject, where the result is assigned to the Subject field
func (e *Envelope) ParseHeaders() error {
	var err error
	if e.Header != nil {
		return errors.New("Headers already parsed")
	}
	buf := bytes.NewBuffer(e.Data.Bytes())
	// find where the header ends, assuming that over 30 kb would be max
	max := maxHeaderChunk
	if buf.Len() < max {
		max = buf.Len()
	}
	// read in the chunk which we'll scan for the header
	chunk := make([]byte, max)
	buf.Read(chunk)
	headerEnd := strings.Index(string(chunk), "\n\n") // the first two new-lines chars are the End Of Header
	if headerEnd > -1 {
		header := chunk[0:headerEnd]
		headerReader := textproto.NewReader(bufio.NewReader(bytes.NewBuffer(header)))
		e.Header, err = headerReader.ReadMIMEHeader()
		if err != nil {
			// decode the subject
			if subject, ok := e.Header["Subject"]; ok {
				e.Subject = MimeHeaderDecode(subject[0])
			}
		}
	} else {
		err = errors.New("header not found")
	}
	return err
}

// Returns a new reader for reading the email contents, including the delivery headers
func (e *Envelope) NewReader() io.Reader {
	return io.MultiReader(
		strings.NewReader(e.DeliveryHeader),
		bytes.NewReader(e.Data.Bytes()),
	)
}

// String converts the email to string.
// Typically, you would want to use the compressor guerrilla.Processor for more efficiency, or use NewReader
func (e *Envelope) String() string {
	return e.DeliveryHeader + e.Data.String()
}

// ResetTransaction is called when the transaction is reset (but save connection)
func (e *Envelope) ResetTransaction() {
	e.MailFrom = Address{}
	e.RcptTo = []Address{}
	// reset the data buffer, keep it allocated
	e.Data.Reset()
}

// Seed is called when used with a new connection, once it's accepted
func (e *Envelope) Reseed(RemoteIP string, clientID uint64) {
	e.Subject = ""
	e.RemoteIP = RemoteIP
	e.Helo = ""
	e.Header = nil
	e.TLS = false
	e.Hashes = make([]string, 0)
	e.DeliveryHeader = ""
	e.Values = make(map[string]interface{})
	e.QueuedId = queuedID(clientID)
}

// PushRcpt adds a recipient email address to the envelope
func (e *Envelope) PushRcpt(addr Address) {
	e.RcptTo = append(e.RcptTo, addr)
}

// Pop removes the last email address that was pushed to the envelope
func (e *Envelope) PopRcpt() Address {
	ret := e.RcptTo[len(e.RcptTo)-1]
	e.RcptTo = e.RcptTo[:len(e.RcptTo)-1]
	return ret
}

var mimeRegex, _ = regexp.Compile(`=\?(.+?)\?([QBqp])\?(.+?)\?=`)

// Decode strings in Mime header format
// eg. =?ISO-2022-JP?B?GyRCIVo9dztSOWJAOCVBJWMbKEI=?=
// This function uses GNU iconv under the hood, for more charset support than in Go's library
func MimeHeaderDecode(str string) string {

	matched := mimeRegex.FindAllStringSubmatch(str, -1)
	var charset, encoding, payload string
	if matched != nil {
		for i := 0; i < len(matched); i++ {
			if len(matched[i]) > 2 {
				charset = matched[i][1]
				encoding = strings.ToUpper(matched[i][2])
				payload = matched[i][3]
				switch encoding {
				case "B":
					str = strings.Replace(
						str,
						matched[i][0],
						MailTransportDecode(payload, "base64", charset),
						1)
				case "Q":
					str = strings.Replace(
						str,
						matched[i][0],
						MailTransportDecode(payload, "quoted-printable", charset),
						1)
				}
			}
		}
	}
	return str
}

// decode from 7bit to 8bit UTF-8
// encodingType can be "base64" or "quoted-printable"
func MailTransportDecode(str string, encodingType string, charset string) string {
	if charset == "" {
		charset = "UTF-8"
	} else {
		charset = strings.ToUpper(charset)
	}
	if encodingType == "base64" {
		str = fromBase64(str)
	} else if encodingType == "quoted-printable" {
		str = fromQuotedP(str)
	}

	if charset != "UTF-8" {
		charset = fixCharset(charset)
		// iconv is pretty good at what it does
		if cd, err := iconv.Open("UTF-8", charset); err == nil {
			defer func() {
				cd.Close()
				if r := recover(); r != nil {
					//logln(1, fmt.Sprintf("Recovered in %v", r))
				}
			}()
			// eg. charset can be "ISO-2022-JP"
			return cd.ConvString(str)
		}

	}
	return str
}

func fromBase64(data string) string {
	buf := bytes.NewBufferString(data)
	decoder := base64.NewDecoder(base64.StdEncoding, buf)
	res, _ := ioutil.ReadAll(decoder)
	return string(res)
}

func fromQuotedP(data string) string {
	buf := bytes.NewBufferString(data)
	decoder := qprintable.NewDecoder(qprintable.BinaryEncoding, buf)
	res, _ := ioutil.ReadAll(decoder)
	return string(res)
}

var charsetRegex, _ = regexp.Compile(`[_:.\/\\]`)

func fixCharset(charset string) string {
	fixed_charset := charsetRegex.ReplaceAllString(charset, "-")
	// Fix charset
	// borrowed from http://squirrelmail.svn.sourceforge.net/viewvc/squirrelmail/trunk/squirrelmail/include/languages.php?revision=13765&view=markup
	// OE ks_c_5601_1987 > cp949
	fixed_charset = strings.Replace(fixed_charset, "ks-c-5601-1987", "cp949", -1)
	// Moz x-euc-tw > euc-tw
	fixed_charset = strings.Replace(fixed_charset, "x-euc", "euc", -1)
	// Moz x-windows-949 > cp949
	fixed_charset = strings.Replace(fixed_charset, "x-windows_", "cp", -1)
	// windows-125x and cp125x charsets
	fixed_charset = strings.Replace(fixed_charset, "windows-", "cp", -1)
	// ibm > cp
	fixed_charset = strings.Replace(fixed_charset, "ibm", "cp", -1)
	// iso-8859-8-i -> iso-8859-8
	fixed_charset = strings.Replace(fixed_charset, "iso-8859-8-i", "iso-8859-8", -1)
	if charset != fixed_charset {
		return fixed_charset
	}
	return charset
}