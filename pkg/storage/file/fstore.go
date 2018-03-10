package file

import (
	"bufio"
	"encoding/gob"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jhillyerd/inbucket/pkg/config"
	"github.com/jhillyerd/inbucket/pkg/log"
	"github.com/jhillyerd/inbucket/pkg/storage"
	"github.com/jhillyerd/inbucket/pkg/stringutil"
)

// Name of index file in each mailbox
const indexFileName = "index.gob"

var (
	// indexMx is locked while reading/writing an index file
	//
	// NOTE: This is a bottleneck because it's a single lock even if we have a
	// million index files
	indexMx = new(sync.RWMutex)

	// dirMx is locked while creating/removing directories
	dirMx = new(sync.Mutex)

	// countChannel is filled with a sequential numbers (0000..9999), which are
	// used by generateID() to generate unique message IDs.  It's global
	// because we only want one regardless of the number of DataStore objects
	countChannel = make(chan int, 10)
)

func init() {
	// Start generator
	go countGenerator(countChannel)
}

// Populates the channel with numbers
func countGenerator(c chan int) {
	for i := 0; true; i = (i + 1) % 10000 {
		c <- i
	}
}

// Store implements DataStore aand is the root of the mail storage
// hiearchy.  It provides access to Mailbox objects
type Store struct {
	hashLock   storage.HashLock
	path       string
	mailPath   string
	messageCap int
}

// New creates a new DataStore object using the specified path
func New(cfg config.DataStoreConfig) storage.Store {
	path := cfg.Path
	if path == "" {
		log.Errorf("No value configured for datastore path")
		return nil
	}
	mailPath := filepath.Join(path, "mail")
	if _, err := os.Stat(mailPath); err != nil {
		// Mail datastore does not yet exist
		if err = os.MkdirAll(mailPath, 0770); err != nil {
			log.Errorf("Error creating dir %q: %v", mailPath, err)
		}
	}
	return &Store{path: path, mailPath: mailPath, messageCap: cfg.MailboxMsgCap}
}

// DefaultStore creates a new DataStore object.  It uses the inbucket.Config object to
// construct it's path.
func DefaultStore() storage.Store {
	cfg := config.GetDataStoreConfig()
	return New(cfg)
}

// MailboxFor retrieves the Mailbox object for a specified email address, if the mailbox
// does not exist, it will attempt to create it.
func (ds *Store) MailboxFor(emailAddress string) (storage.Mailbox, error) {
	name, err := stringutil.ParseMailboxName(emailAddress)
	if err != nil {
		return nil, err
	}
	dir := stringutil.HashMailboxName(name)
	s1 := dir[0:3]
	s2 := dir[0:6]
	path := filepath.Join(ds.mailPath, s1, s2, dir)
	indexPath := filepath.Join(path, indexFileName)

	return &Mailbox{store: ds, name: name, dirName: dir, path: path,
		indexPath: indexPath}, nil
}

// AllMailboxes returns a slice with all Mailboxes
func (ds *Store) AllMailboxes() ([]storage.Mailbox, error) {
	mailboxes := make([]storage.Mailbox, 0, 100)
	infos1, err := ioutil.ReadDir(ds.mailPath)
	if err != nil {
		return nil, err
	}
	// Loop over level 1 directories
	for _, inf1 := range infos1 {
		if inf1.IsDir() {
			l1 := inf1.Name()
			infos2, err := ioutil.ReadDir(filepath.Join(ds.mailPath, l1))
			if err != nil {
				return nil, err
			}
			// Loop over level 2 directories
			for _, inf2 := range infos2 {
				if inf2.IsDir() {
					l2 := inf2.Name()
					infos3, err := ioutil.ReadDir(filepath.Join(ds.mailPath, l1, l2))
					if err != nil {
						return nil, err
					}
					// Loop over mailboxes
					for _, inf3 := range infos3 {
						if inf3.IsDir() {
							mbdir := inf3.Name()
							mbpath := filepath.Join(ds.mailPath, l1, l2, mbdir)
							idx := filepath.Join(mbpath, indexFileName)
							mb := &Mailbox{store: ds, dirName: mbdir, path: mbpath,
								indexPath: idx}
							mailboxes = append(mailboxes, mb)
						}
					}
				}
			}
		}
	}

	return mailboxes, nil
}

// LockFor returns the RWMutex for this mailbox, or an error.
func (ds *Store) LockFor(emailAddress string) (*sync.RWMutex, error) {
	name, err := stringutil.ParseMailboxName(emailAddress)
	if err != nil {
		return nil, err
	}
	hash := stringutil.HashMailboxName(name)
	return ds.hashLock.Get(hash), nil
}

// Mailbox implements Mailbox, manages the mail for a specific user and
// correlates to a particular directory on disk.
type Mailbox struct {
	store       *Store
	name        string
	dirName     string
	path        string
	indexLoaded bool
	indexPath   string
	messages    []*Message
}

// Name of the mailbox
func (mb *Mailbox) Name() string {
	return mb.name
}

// String renders the name and directory path of the mailbox
func (mb *Mailbox) String() string {
	return mb.name + "[" + mb.dirName + "]"
}

// GetMessages scans the mailbox directory for .gob files and decodes them into
// a slice of Message objects.
func (mb *Mailbox) GetMessages() ([]storage.Message, error) {
	if !mb.indexLoaded {
		if err := mb.readIndex(); err != nil {
			return nil, err
		}
	}

	messages := make([]storage.Message, len(mb.messages))
	for i, m := range mb.messages {
		messages[i] = m
	}
	return messages, nil
}

// GetMessage decodes a single message by Id and returns a Message object
func (mb *Mailbox) GetMessage(id string) (storage.Message, error) {
	if !mb.indexLoaded {
		if err := mb.readIndex(); err != nil {
			return nil, err
		}
	}

	if id == "latest" && len(mb.messages) != 0 {
		return mb.messages[len(mb.messages)-1], nil
	}

	for _, m := range mb.messages {
		if m.Fid == id {
			return m, nil
		}
	}

	return nil, storage.ErrNotExist
}

// Purge deletes all messages in this mailbox
func (mb *Mailbox) Purge() error {
	mb.messages = mb.messages[:0]
	return mb.writeIndex()
}

// readIndex loads the mailbox index data from disk
func (mb *Mailbox) readIndex() error {
	// Clear message slice, open index
	mb.messages = mb.messages[:0]
	// Lock for reading
	indexMx.RLock()
	defer indexMx.RUnlock()
	// Check if index exists
	if _, err := os.Stat(mb.indexPath); err != nil {
		// Does not exist, but that's not an error in our world
		log.Tracef("Index %v does not exist (yet)", mb.indexPath)
		mb.indexLoaded = true
		return nil
	}
	file, err := os.Open(mb.indexPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); err != nil {
			log.Errorf("Failed to close %q: %v", mb.indexPath, err)
		}
	}()

	// Decode gob data
	dec := gob.NewDecoder(bufio.NewReader(file))
	for {
		msg := new(Message)
		if err = dec.Decode(msg); err != nil {
			if err == io.EOF {
				// It's OK to get an EOF here
				break
			}
			return fmt.Errorf("Corrupt mailbox %q: %v", mb.indexPath, err)
		}
		msg.mailbox = mb
		mb.messages = append(mb.messages, msg)
	}

	mb.indexLoaded = true
	return nil
}

// writeIndex overwrites the index on disk with the current mailbox data
func (mb *Mailbox) writeIndex() error {
	// Lock for writing
	indexMx.Lock()
	defer indexMx.Unlock()
	if len(mb.messages) > 0 {
		// Ensure mailbox directory exists
		if err := mb.createDir(); err != nil {
			return err
		}
		// Open index for writing
		file, err := os.Create(mb.indexPath)
		if err != nil {
			return err
		}
		writer := bufio.NewWriter(file)
		// Write each message and then flush
		enc := gob.NewEncoder(writer)
		for _, m := range mb.messages {
			err = enc.Encode(m)
			if err != nil {
				_ = file.Close()
				return err
			}
		}
		if err := writer.Flush(); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			log.Errorf("Failed to close %q: %v", mb.indexPath, err)
			return err
		}
	} else {
		// No messages, delete index+maildir
		log.Tracef("Removing mailbox %v", mb.path)
		return mb.removeDir()
	}

	return nil
}

// createDir checks for the presence of the path for this mailbox, creates it if needed
func (mb *Mailbox) createDir() error {
	dirMx.Lock()
	defer dirMx.Unlock()
	if _, err := os.Stat(mb.path); err != nil {
		if err := os.MkdirAll(mb.path, 0770); err != nil {
			log.Errorf("Failed to create directory %v, %v", mb.path, err)
			return err
		}
	}
	return nil
}

// removeDir removes the mailbox, plus empty higher level directories
func (mb *Mailbox) removeDir() error {
	dirMx.Lock()
	defer dirMx.Unlock()
	// remove mailbox dir, including index file
	if err := os.RemoveAll(mb.path); err != nil {
		return err
	}
	// remove parents if empty
	dir := filepath.Dir(mb.path)
	if removeDirIfEmpty(dir) {
		removeDirIfEmpty(filepath.Dir(dir))
	}
	return nil
}

// removeDirIfEmpty will remove the specified directory if it contains no files or directories.
// Caller should hold dirMx.  Returns true if dir was removed.
func removeDirIfEmpty(path string) (removed bool) {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	files, err := f.Readdirnames(0)
	_ = f.Close()
	if err != nil {
		return false
	}
	if len(files) > 0 {
		// Dir not empty
		return false
	}
	log.Tracef("Removing dir %v", path)
	err = os.Remove(path)
	if err != nil {
		log.Errorf("Failed to remove %q: %v", path, err)
		return false
	}
	return true
}

// generatePrefix converts a Time object into the ISO style format we use
// as a prefix for message files.  Note:  It is used directly by unit
// tests.
func generatePrefix(date time.Time) string {
	return date.Format("20060102T150405")
}

// generateId adds a 4-digit unique number onto the end of the string
// returned by generatePrefix()
func generateID(date time.Time) string {
	return generatePrefix(date) + "-" + fmt.Sprintf("%04d", <-countChannel)
}
