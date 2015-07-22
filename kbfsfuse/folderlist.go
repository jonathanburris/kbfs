package main

import (
	"log"
	"os"
	"strings"
	"sync"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"github.com/keybase/client/protocol/go"
	"github.com/keybase/kbfs/libkbfs"
	"golang.org/x/net/context"
)

// FolderList is a node that can list all of the logged-in user's
// favorite top-level folders, on either a public or private basis.
type FolderList struct {
	fs *FS
	// only accept public folders
	public bool

	mu      sync.Mutex
	folders map[string]*Dir
}

var _ fs.Node = (*FolderList)(nil)

// Attr implements the fs.Node interface.
func (*FolderList) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | 0755
	return nil
}

var _ fs.NodeRequestLookuper = (*FolderList)(nil)

// Lookup implements the fs.NodeRequestLookuper interface.
func (fl *FolderList) Lookup(ctx context.Context, req *fuse.LookupRequest, resp *fuse.LookupResponse) (node fs.Node, err error) {
	defer func() { fl.fs.reportErr(err) }()
	fl.mu.Lock()
	defer fl.mu.Unlock()

	if req.Name == libkbfs.ErrorFile {
		resp.EntryValid = 0
		n := &ErrorFile{
			fs: fl.fs,
		}
		return n, nil
	}

	if child, ok := fl.folders[req.Name]; ok {
		return child, nil
	}

	dh, err := libkbfs.ParseTlfHandle(ctx, fl.fs.config, req.Name)
	if err != nil {
		return nil, err
	}

	if fl.public && !dh.HasPublic() {
		// no public folder exists for this folder
		return nil, fuse.ENOENT
	}

	if canon := dh.ToString(ctx, fl.fs.config); canon != req.Name {
		n := &Alias{
			canon: canon,
		}
		return n, nil
	}

	if fl.public {
		dh = &libkbfs.TlfHandle{
			Writers: dh.Writers,
			Readers: []keybase1.UID{keybase1.PublicUID},
		}
	}

	rootNode, _, err := fl.fs.config.KBFSOps().GetOrCreateRootNodeForHandle(ctx, dh, libkbfs.MasterBranch)
	if err != nil {
		// TODO make errors aware of fuse
		return nil, err
	}

	folderBranch := rootNode.GetFolderBranch()
	folder := &Folder{
		fs:           fl.fs,
		list:         fl,
		name:         req.Name,
		folderBranch: folderBranch,
		nodes:        map[libkbfs.NodeID]fs.Node{},
	}

	// TODO unregister all at unmount
	if err := fl.fs.config.Notifier().RegisterForChanges([]libkbfs.FolderBranch{folderBranch}, folder); err != nil {
		return nil, err
	}

	child := newDir(folder, rootNode)
	folder.nodes[rootNode.GetID()] = child

	fl.folders[req.Name] = child
	return child, nil
}

func (fl *FolderList) forgetFolder(f *Folder) {
	fl.mu.Lock()
	defer fl.mu.Unlock()

	if err := fl.fs.config.Notifier().UnregisterFromChanges([]libkbfs.FolderBranch{f.folderBranch}, f); err != nil {
		log.Printf("cannot unregister change notifier for folder %q: %v", f.name, err)
	}
	delete(fl.folders, f.name)
}

var _ fs.Handle = (*FolderList)(nil)

var _ fs.HandleReadDirAller = (*FolderList)(nil)

func (fl *FolderList) getDirent(ctx context.Context, work <-chan libkbfs.TlfID, results chan<- fuse.Dirent) error {
	for {
		select {
		case tlfID, ok := <-work:
			if !ok {
				return nil
			}
			_, _, dh, err := fl.fs.config.KBFSOps().GetRootNode(ctx,
				libkbfs.FolderBranch{Tlf: tlfID, Branch: libkbfs.MasterBranch})
			if err != nil {
				return err
			}
			name := dh.ToString(ctx, fl.fs.config)
			if fl.public {
				const publicSuffix = libkbfs.ReaderSep + libkbfs.PublicUIDName
				// strip off the public portion
				name = strings.TrimSuffix(name, publicSuffix)
			}
			results <- fuse.Dirent{
				Type: fuse.DT_Dir,
				Name: name,
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// ReadDirAll implements the ReadDirAll interface.
func (fl *FolderList) ReadDirAll(ctx context.Context) (res []fuse.Dirent, err error) {
	defer func() { fl.fs.reportErr(err) }()
	favs, err := fl.fs.config.KBFSOps().GetFavorites(ctx)
	if err != nil {
		return nil, err
	}
	work := make(chan libkbfs.TlfID)
	results := make(chan fuse.Dirent)
	errCh := make(chan error, 1)
	const maxWorkers = 10
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for i := 0; i < maxWorkers && i < len(favs); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fl.getDirent(ctx, work, results); err != nil {
				select {
				case errCh <- err:
				default:
				}
			}
		}()
	}

	go func() {
		// feed work
		for _, tlfID := range favs {
			if fl.public != tlfID.IsPublic() {
				continue
			}
			work <- tlfID
		}
		close(work)
		wg.Wait()
		// workers are done
		close(results)
	}()

outer:
	for {
		select {
		case dirent, ok := <-results:
			if !ok {
				break outer
			}
			res = append(res, dirent)
		case err := <-errCh:
			return nil, err
		}
	}
	return res, nil
}
