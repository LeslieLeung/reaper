package scm

import (
	"errors"
	"strings"
)

type Repository struct {
	Host  string
	Owner string
	Name  string
}

var (
	ErrInvalidURL = errors.New("invalid url")
)

func NewRepository(url string) (*Repository, error) {
	r := &Repository{}
	l := strings.Split(url, "/")
	if len(l) < 3 {
		return nil, ErrInvalidURL
	}
	r.Host = l[0]
	r.Owner = l[1]
	r.Name = l[2]
	return r, nil
}
