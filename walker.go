package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"unicode/utf8"
)

var ErrInvalidText = errors.New("unavailable encoding")

type InternalError struct {
	path string
	e    error
}

func (ei *InternalError) Error() string {
	return fmt.Sprintf("internal error:%v:%s", ei.e, ei.path)
}

type Line struct {
	Num uint
	Str string
}

type LineQueue struct {
	ls []*Line
}

func (lq *LineQueue) Len() int     { return len(lq.ls) }
func (lq *LineQueue) Push(l *Line) { lq.ls = append(lq.ls, l) }
func (lq *LineQueue) Pop() *Line {
	// believe to do not access out of bounds.
	l := lq.ls[0]
	lq.ls = lq.ls[1:]
	return l
}
func (lq *LineQueue) PopAll() []*Line {
	lines := make([]*Line, 0, len(lq.ls))
	for lq.Len() != 0 {
		lines = append(lines, lq.Pop())
	}
	return lines
}
func (lq *LineQueue) Reset() {
	if lq.Len() != 0 {
		lq.ls = lq.ls[:0]
	}
}

// change to struct{ index int, pos int, lines []*Line }?
type Context struct {
	line   *Line
	before []*Line
	after  []*Line
}

func FprintContexts(writer io.Writer, prefix string, cs []*Context) error {
	if cs == nil {
		return nil
	}
	var err error
	f := func(ls []*Line) {
		for _, l := range ls {
			_, err = fmt.Fprintf(writer, "%s%d-%s\n", prefix, l.Num, l.Str)
		}
	}
	for _, c := range cs {
		f(c.before)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(writer, "%s%d:%s\n", prefix, c.line.Num, c.line.Str)
		if err != nil {
			return err
		}
		f(c.after)
		if err != nil {
			return err
		}
	}
	return nil
}

type File struct {
	path string
	cs   []*Context
}

func (f *File) Fprint(writer io.Writer) error {
	var err error
	if len(f.cs) == 0 {
		return nil
	}
	_, err = fmt.Fprintln(writer, f.path)
	if err != nil {
		return err
	}
	err = FprintContexts(writer, "", f.cs)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(writer)
	if err != nil {
		return err
	}
	return nil
}

func (f *File) FprintVerbose(writer io.Writer) error {
	var err error
	if len(f.cs) == 0 {
		return nil
	}
	err = FprintContexts(writer, f.path, f.cs)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(writer)
	if err != nil {
		return err
	}
	return nil
}

func FprintFiles(writer io.Writer, fs ...*File) error {
	var err error
	for i := range fs {
		err = fs[i].Fprint(writer)
		if err != nil {
			return err
		}
	}
	return nil
}

func FprintFilesVerbose(writer io.Writer, fs ...*File) error {
	var err error
	for i := range fs {
		err = fs[i].FprintVerbose(writer)
		if err != nil {
			return err
		}
	}
	return nil
}

type Walker struct {
	fileQueue chan string
	dirQueue  chan []string

	regexp *regexp.Regexp

	nworker       int
	checked       map[string]bool
	mu            sync.Mutex
	wg            sync.WaitGroup
	once          sync.Once
	internalError error

	log *log.Logger
}

func NewWalker() *Walker {
	nworker := runtime.NumCPU() / 4
	if nworker < 2 {
		nworker = 2
	}
	return &Walker{
		fileQueue:     make(chan string, 128),
		dirQueue:      make(chan []string, nworker),
		nworker:       nworker,
		checked:       make(map[string]bool),
		log:           log.New(ioutil.Discard, Name+":", 0),
		internalError: nil,
	}
}

func (w *Walker) SetLogOutput(writer io.Writer) { w.log.SetOutput(writer) }

func (w *Walker) setInternalError(err error, path string) {
	w.once.Do(func() { w.internalError = &InternalError{e: err, path: path} })
}

func (w *Walker) sendQueue(paths ...string) {
	var dirs []string
	for i := range paths {
		abs, err := filepath.Abs(paths[i])
		if err != nil {
			w.setInternalError(err, abs)
			w.log.Printf("[Err]:%v", err)
			continue
		}
		fi, err := os.Stat(abs)
		if err != nil {
			w.setInternalError(err, abs)
			w.log.Printf("[Errr]:%v", err)
			continue
		}
		if fi.IsDir() {
			dirs = append(dirs, abs)
		} else if fi.Mode().IsRegular() {
			w.wg.Add(1)
			w.fileQueue <- abs
		}
	}
	w.wg.Add(1)
	w.dirQueue <- dirs
}

func (w *Walker) Start(pat string, nlines int, paths ...string) (<-chan *File, func() error, error) {
	var re *regexp.Regexp
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil, nil, err
	}
	w.regexp = re

	resultQueue := make(chan *File, cap(w.fileQueue))
	done := make(chan struct{})
	wait := func() error {
		w.wg.Wait()
		return w.internalError
	}

	for i := 0; i != w.nworker; i++ {
		go w.dirWalker(done)
		go w.fileWalker(done, resultQueue, nlines)
	}
	w.sendQueue(paths...)
	go func() {
		w.wg.Wait()
		close(done)
		close(resultQueue)
	}()
	return resultQueue, wait, nil
}

// for goroutine
// send tasks to {file,dir}Queue
func (w *Walker) dirWalker(done <-chan struct{}) {
	var nextDirs []string
	var dirs []string
	for ; true; w.wg.Done() {
		select {
		case <-done:
			return
		case dirs = <-w.dirQueue:
			for i := range dirs {
				fis, err := ioutil.ReadDir(dirs[i])
				if err != nil {
					w.setInternalError(err, dirs[i])
					w.log.Printf("[Err]:%s:%v", dirs[i], err)
					if os.IsNotExist(err) || os.IsPermission(err) {
						continue
					}
					// unexpected error
					panic(err)
				}
				for _, fi := range fis {
					if fi.Mode().IsRegular() {
						w.wg.Add(1)
						w.fileQueue <- filepath.Join(dirs[i], fi.Name())
					} else if fi.IsDir() {
						nextDirs = append(nextDirs, filepath.Join(dirs[i], fi.Name()))
					}
				}
			}
			if len(nextDirs) != 0 {
				w.wg.Add(1)
				w.dirQueue <- nextDirs
				nextDirs = nextDirs[:0]
			}
		}
	}
}

// for goroutine
func (w *Walker) fileWalker(done <-chan struct{}, resultQueue chan<- *File, nlines int) {
	lq := new(LineQueue)
	var file string
	var err error
	var cs []*Context
	for ; true; w.wg.Done() {
		select {
		case <-done:
			return
		case file = <-w.fileQueue:
			w.mu.Lock()
			if w.checked[file] {
				w.mu.Unlock()
				continue
			}
			w.checked[file] = true
			w.mu.Unlock()

			cs, err = w.readFile(file, lq, nlines)
			if err != nil {
				w.setInternalError(err, file)
				w.log.Printf("[Err]:%s:%v", file, err)
				if os.IsNotExist(err) || os.IsPermission(err) {
					continue
				}
				if err == bufio.ErrTooLong {
					continue
				}
				if err == ErrInvalidText {
					continue
				}
				// unexpected error
				panic(err)
			}
			w.log.Println(file)
			if len(cs) != 0 {
				resultQueue <- &File{
					path: file,
					cs:   cs,
				}
			}
		}
	}
}

// TODO? readFile(f *File, file string) error
func (w *Walker) readFile(file string, lq *LineQueue, nlines int) ([]*Context, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var cs []*Context
	var c = new(Context)
	var txt string
	var i uint
	var matched bool

	var csAdd func()
	if nlines < 1 {
		csAdd = func() {
			if matched {
				cs = append(cs, &Context{
					line:   &Line{i, txt},
					before: []*Line{},
					after:  []*Line{},
				})
			}
		}
	} else {
		defer lq.Reset()
		csAdd = func() {
			if c.line != nil {
				if matched {
					c.after = lq.PopAll()
					cs = append(cs, c)
					c = &Context{
						before: []*Line{},
						line:   &Line{i, txt},
						after:  []*Line{},
					}
					return
				}
				if lq.Len() == nlines {
					c.after = lq.PopAll()
					cs = append(cs, c)
					c = new(Context)
				}
			} else if matched {
				c.before = lq.PopAll()
				c.line = &Line{i, txt}
				return
			}
			if lq.Len() == nlines {
				lq.Pop()
			}
			lq.Push(&Line{i, txt})
		}
	}

	sc := bufio.NewScanner(f)
	for i = uint(1); sc.Scan(); i++ {
		if i == 0 {
			return nil, errors.New("too many lines")
		}
		txt = sc.Text()
		if !utf8.ValidString(txt) {
			return nil, ErrInvalidText
		}
		matched = w.regexp.MatchString(txt)
		csAdd()
	}
	if err = sc.Err(); err != nil {
		return nil, err
	}

	// append last one for nlines > 1
	if c.line != nil {
		c.after = lq.PopAll()
		cs = append(cs, c)
	}
	return cs, nil
}
