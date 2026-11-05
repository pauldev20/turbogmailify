package bridge

import (
	"bufio"
	"bytes"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"
)

func readHeader(raw []byte) (mail.Header, error) {
	th, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
	return mail.Header{Header: message.Header{Header: th}}, err
}
