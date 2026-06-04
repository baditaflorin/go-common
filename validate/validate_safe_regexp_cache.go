package validate

import (
	"regexp"
)

// We use a plain map protected by a channel semaphore to avoid
// importing sync explicitly (it's in stdlib but let's be explicit).
type safeRegexpCache struct {
	items map[string]*regexp.Regexp
	sem   chan struct{}
}

func (c *safeRegexpCache) compile(pattern string) (*regexp.Regexp, error) {
	<-c.sem
	defer func() { c.sem <- struct{}{} }()
	if re, ok := c.items[pattern]; ok {
		return re, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	c.items[pattern] = re
	return re, nil
}
