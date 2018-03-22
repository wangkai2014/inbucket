package file

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	"github.com/jhillyerd/inbucket/pkg/config"
	"github.com/jhillyerd/inbucket/pkg/log"
	"github.com/jhillyerd/inbucket/pkg/policy"
	"github.com/jhillyerd/inbucket/pkg/storage"
	"github.com/jhillyerd/inbucket/pkg/stringutil"
)

// Name of index file in each mailbox
const indexFileName = "index.gob"

var (
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
func New(cfg config.Storage) storage.Store {
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

// AddMessage adds a message to the specified mailbox.
func (fs *Store) AddMessage(m storage.Message) (id string, err error) {
	mb, err := fs.mbox(m.Mailbox())
	if err != nil {
		return "", err
	}
	mb.Lock()
	defer mb.Unlock()
	r, err := m.Source()
	if err != nil {
		return "", err
	}
	// Create a new message.
	fm, err := mb.newMessage()
	if err != nil {
		return "", err
	}
	// Ensure mailbox directory exists.
	if err := mb.createDir(); err != nil {
		return "", err
	}
	// Write the message content
	file, err := os.Create(fm.rawPath())
	if err != nil {
		return "", err
	}
	w := bufio.NewWriter(file)
	size, err := io.Copy(w, r)
	if err != nil {
		// Try to remove the file
		_ = file.Close()
		_ = os.Remove(fm.rawPath())
		return "", err
	}
	_ = r.Close()
	if err := w.Flush(); err != nil {
		// Try to remove the file
		_ = file.Close()
		_ = os.Remove(fm.rawPath())
		return "", err
	}
	if err := file.Close(); err != nil {
		// Try to remove the file
		_ = os.Remove(fm.rawPath())
		return "", err
	}
	// Update the index.
	fm.Fdate = m.Date()
	fm.Ffrom = m.From()
	fm.Fto = m.To()
	fm.Fsize = size
	fm.Fsubject = m.Subject()
	mb.messages = append(mb.messages, fm)
	if err := mb.writeIndex(); err != nil {
		// Try to remove the file
		_ = os.Remove(fm.rawPath())
		return "", err
	}
	return fm.Fid, nil
}

// GetMessage returns the messages in the named mailbox, or an error.
func (fs *Store) GetMessage(mailbox, id string) (storage.Message, error) {
	mb, err := fs.mbox(mailbox)
	if err != nil {
		return nil, err
	}
	mb.RLock()
	defer mb.RUnlock()
	return mb.getMessage(id)
}

// GetMessages returns the messages in the named mailbox, or an error.
func (fs *Store) GetMessages(mailbox string) ([]storage.Message, error) {
	mb, err := fs.mbox(mailbox)
	if err != nil {
		return nil, err
	}
	mb.RLock()
	defer mb.RUnlock()
	return mb.getMessages()
}

// RemoveMessage deletes a message by ID from the specified mailbox.
func (fs *Store) RemoveMessage(mailbox, id string) error {
	mb, err := fs.mbox(mailbox)
	if err != nil {
		return err
	}
	mb.Lock()
	defer mb.Unlock()
	return mb.removeMessage(id)
}

// PurgeMessages deletes all messages in the named mailbox, or returns an error.
func (fs *Store) PurgeMessages(mailbox string) error {
	mb, err := fs.mbox(mailbox)
	if err != nil {
		return err
	}
	mb.Lock()
	defer mb.Unlock()
	return mb.purge()
}

// VisitMailboxes accepts a function that will be called with the messages in each mailbox while it
// continues to return true.
func (fs *Store) VisitMailboxes(f func([]storage.Message) (cont bool)) error {
	infos1, err := ioutil.ReadDir(fs.mailPath)
	if err != nil {
		return err
	}
	// Loop over level 1 directories
	for _, inf1 := range infos1 {
		if inf1.IsDir() {
			l1 := inf1.Name()
			infos2, err := ioutil.ReadDir(filepath.Join(fs.mailPath, l1))
			if err != nil {
				return err
			}
			// Loop over level 2 directories
			for _, inf2 := range infos2 {
				if inf2.IsDir() {
					l2 := inf2.Name()
					infos3, err := ioutil.ReadDir(filepath.Join(fs.mailPath, l1, l2))
					if err != nil {
						return err
					}
					// Loop over mailboxes
					for _, inf3 := range infos3 {
						if inf3.IsDir() {
							mb := fs.mboxFromHash(inf3.Name())
							mb.RLock()
							msgs, err := mb.getMessages()
							mb.RUnlock()
							if err != nil {
								return err
							}
							if !f(msgs) {
								return nil
							}
						}
					}
				}
			}
		}
	}
	return nil
}

// mbox returns the named mailbox.
func (fs *Store) mbox(mailbox string) (*mbox, error) {
	name, err := policy.ParseMailboxName(mailbox)
	if err != nil {
		return nil, err
	}
	hash := stringutil.HashMailboxName(name)
	s1 := hash[0:3]
	s2 := hash[0:6]
	path := filepath.Join(fs.mailPath, s1, s2, hash)
	indexPath := filepath.Join(path, indexFileName)
	return &mbox{
		RWMutex:   fs.hashLock.Get(hash),
		store:     fs,
		name:      name,
		dirName:   hash,
		path:      path,
		indexPath: indexPath,
	}, nil
}

// mboxFromPath constructs a mailbox based on name hash.
func (fs *Store) mboxFromHash(hash string) *mbox {
	s1 := hash[0:3]
	s2 := hash[0:6]
	path := filepath.Join(fs.mailPath, s1, s2, hash)
	indexPath := filepath.Join(path, indexFileName)
	return &mbox{
		RWMutex:   fs.hashLock.Get(hash),
		store:     fs,
		dirName:   hash,
		path:      path,
		indexPath: indexPath,
	}
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
