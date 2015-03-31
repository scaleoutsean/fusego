// Copyright 2015 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package fuseops contains implementations of the fuse.Op interface that may
// be returned by fuse.Connection.ReadOp. See documentation in that package for
// more.
package fuseops

import (
	"os"
	"time"

	"github.com/jacobsa/bazilfuse"
	"golang.org/x/net/context"
)

// A common interface implemented by all ops in this package. Use a type switch
// to find particular concrete types, responding with fuse.ENOSYS if a type is
// not supported.
type Op interface {
	// Return the fields common to all operations.
	Header() OpHeader

	// A context that can be used for long-running operations.
	Context() context.Context

	// Repond to the operation with the supplied error. If there is no error, set
	// any necessary output fields and then call Respond(nil).
	Respond(error)
}

////////////////////////////////////////////////////////////////////////
// Setup
////////////////////////////////////////////////////////////////////////

// Sent once when mounting the file system. It must succeed in order for the
// mount to succeed.
type InitOp struct {
	commonOp

	maxReadahead uint32
}

func (o *InitOp) Respond(err error) {
	if err != nil {
		o.commonOp.respondErr(err)
		return
	}

	resp := bazilfuse.InitResponse{}

	// Ask the Linux kernel for larger write requests.
	//
	// As of 2015-03-26, the behavior in the kernel is:
	//
	//  *  (http://goo.gl/jMKHMZ, http://goo.gl/XTF4ZH) Cap the max write size at
	//     the maximum of 4096 and init_response->max_write.
	//
	//  *  (http://goo.gl/gEIvHZ) If FUSE_BIG_WRITES isn't set, don't return more
	//     than one page.
	//
	//  *  (http://goo.gl/4RLhxZ, http://goo.gl/hi0Cm2) Never write more than
	//     FUSE_MAX_PAGES_PER_REQ pages (128 KiB on x86).
	//
	// 4 KiB is crazy small. Ask for significantly more, and take what the kernel
	// will give us.
	const maxWrite = 1 << 21
	resp.Flags |= bazilfuse.InitBigWrites
	resp.MaxWrite = maxWrite

	// Ask the Linux kernel for larger read requests.
	//
	// As of 2015-03-26, the behavior in the kernel is:
	//
	//  *  (http://goo.gl/bQ1f1i, http://goo.gl/HwBrR6) Set the local variable
	//     ra_pages to be init_response->max_readahead divided by the page size.
	//
	//  *  (http://goo.gl/gcIsSh, http://goo.gl/LKV2vA) Set
	//     backing_dev_info::ra_pages to the min of that value and what was sent
	//     in the request's max_readahead field.
	//
	//  *  (http://goo.gl/u2SqzH) Use backing_dev_info::ra_pages when deciding
	//     how much to read ahead.
	//
	//  *  (http://goo.gl/JnhbdL) Don't read ahead at all if that field is zero.
	//
	// Reading a page at a time is a drag. Ask for as much as the kernel is
	// willing to give us.
	resp.MaxReadahead = o.maxReadahead

	// Respond.
	o.commonOp.logger.Printf("Responding: %v", &resp)
	o.r.(*bazilfuse.InitRequest).Respond(&resp)
}

////////////////////////////////////////////////////////////////////////
// Inodes
////////////////////////////////////////////////////////////////////////

// Look up a child by name within a parent directory. The kernel sends this
// when resolving user paths to dentry structs, which are then cached.
type LookUpInodeOp struct {
	commonOp

	// The ID of the directory inode to which the child belongs.
	Parent InodeID

	// The name of the child of interest, relative to the parent. For example, in
	// this directory structure:
	//
	//     foo/
	//         bar/
	//             baz
	//
	// the file system may receive a request to look up the child named "bar" for
	// the parent foo/.
	Name string

	// The resulting entry. Must be filled out by the file system.
	//
	// The lookup count for the inode is implicitly incremented. See notes on
	// ForgetInodeOp for more information.
	Entry ChildInodeEntry
}

func (o *LookUpInodeOp) Respond(err error) {
	if err != nil {
		o.commonOp.respondErr(err)
		return
	}

	resp := bazilfuse.LookupResponse{}
	convertChildInodeEntry(&o.Entry, &resp)

	o.commonOp.logger.Printf("Responding: %v", &resp)
	o.r.(*bazilfuse.LookupRequest).Respond(&resp)
}

// Refresh the attributes for an inode whose ID was previously returned in a
// LookUpInodeOp. The kernel sends this when the FUSE VFS layer's cache of
// inode attributes is stale. This is controlled by the AttributesExpiration
// field of ChildInodeEntry, etc.
type GetInodeAttributesOp struct {
	commonOp

	// The inode of interest.
	Inode InodeID

	// Set by the file system: attributes for the inode, and the time at which
	// they should expire. See notes on ChildInodeEntry.AttributesExpiration for
	// more.
	Attributes           InodeAttributes
	AttributesExpiration time.Time
}

func (o *GetInodeAttributesOp) Respond(err error) {
	if err != nil {
		o.commonOp.respondErr(err)
		return
	}

	resp := bazilfuse.GetattrResponse{
		Attr:      convertAttributes(o.Inode, o.Attributes),
		AttrValid: convertExpirationTime(o.AttributesExpiration),
	}

	o.commonOp.logger.Printf("Responding: %v", &resp)
	o.r.(*bazilfuse.GetattrRequest).Respond(&resp)
}

// Change attributes for an inode.
//
// The kernel sends this for obvious cases like chmod(2), and for less obvious
// cases like ftrunctate(2).
type SetInodeAttributesOp struct {
	commonOp

	// The inode of interest.
	Inode InodeID

	// The attributes to modify, or nil for attributes that don't need a change.
	Size  *uint64
	Mode  *os.FileMode
	Atime *time.Time
	Mtime *time.Time

	// Set by the file system: the new attributes for the inode, and the time at
	// which they should expire. See notes on
	// ChildInodeEntry.AttributesExpiration for more.
	Attributes           InodeAttributes
	AttributesExpiration time.Time
}

func (o *SetInodeAttributesOp) Respond(err error) {
	if err != nil {
		o.commonOp.respondErr(err)
		return
	}

	resp := bazilfuse.SetattrResponse{
		Attr:      convertAttributes(o.Inode, o.Attributes),
		AttrValid: convertExpirationTime(o.AttributesExpiration),
	}

	o.commonOp.logger.Printf("Responding: %v", &resp)
	o.r.(*bazilfuse.SetattrRequest).Respond(&resp)
}

// Decrement the reference count for an inode ID previously issued by the file
// system.
//
// The comments for the ops that implicitly increment the reference count
// contain a note of this (but see also the note about the root inode below).
// For example, LookUpInodeOp and MkDirOp. The authoritative source is the
// libfuse documentation, which states that any op that returns
// fuse_reply_entry fuse_reply_create implicitly increments (cf.
// http://goo.gl/o5C7Dx).
//
// If the reference count hits zero, the file system can forget about that ID
// entirely, and even re-use it in future responses. The kernel guarantees that
// it will not otherwise use it again.
//
// The reference count corresponds to fuse_inode::nlookup
// (http://goo.gl/ut48S4). Some examples of where the kernel manipulates it:
//
//  *  (http://goo.gl/vPD9Oh) Any caller to fuse_iget increases the count.
//  *  (http://goo.gl/B6tTTC) fuse_lookup_name calls fuse_iget.
//  *  (http://goo.gl/IlcxWv) fuse_create_open calls fuse_iget.
//  *  (http://goo.gl/VQMQul) fuse_dentry_revalidate increments after
//     revalidating.
//
// In contrast to all other inodes, RootInodeID begins with an implicit
// reference count of one, without a corresponding op to increase it. (There
// could be no such op, because the root cannot be referred to by name.) Code
// walk:
//
//  *  (http://goo.gl/gWAheU) fuse_fill_super calls fuse_get_root_inode.
//
//  *  (http://goo.gl/AoLsbb) fuse_get_root_inode calls fuse_iget without
//     sending any particular request.
//
//  *  (http://goo.gl/vPD9Oh) fuse_iget increments nlookup.
//
// File systems should not make assumptions about whether they will or will not
// receive a ForgetInodeOp for the root inode. Experimentally, OS X seems to
// never send one, while Linux appears to send one only sometimes. (Cf.
// http://goo.gl/EUbxEg, fuse-devel thread "Root inode lookup count").
type ForgetInodeOp struct {
	commonOp

	// The inode whose reference count should be decremented.
	Inode InodeID

	// The amount to decrement the reference count.
	N uint64
}

func (o *ForgetInodeOp) Respond(err error) {
	if err != nil {
		o.commonOp.respondErr(err)
		return
	}

	o.commonOp.logger.Printf("Responding OK to ForgetInodeOp")
	o.r.(*bazilfuse.ForgetRequest).Respond()
}

////////////////////////////////////////////////////////////////////////
// Inode creation
////////////////////////////////////////////////////////////////////////

// Create a directory inode as a child of an existing directory inode. The
// kernel sends this in response to a mkdir(2) call.
//
// The kernel appears to verify the name doesn't already exist (mkdir calls
// mkdirat calls user_path_create calls filename_create, which verifies:
// http://goo.gl/FZpLu5). But volatile file systems and paranoid non-volatile
// file systems should check for the reasons described below on CreateFile.
type MkDirOp struct {
	commonOp

	// The ID of parent directory inode within which to create the child.
	Parent InodeID

	// The name of the child to create, and the mode with which to create it.
	Name string
	Mode os.FileMode

	// Set by the file system: information about the inode that was created.
	//
	// The lookup count for the inode is implicitly incremented. See notes on
	// ForgetInodeOp for more information.
	Entry ChildInodeEntry
}

func (o *MkDirOp) Respond(err error) {
	if err != nil {
		o.commonOp.respondErr(err)
		return
	}

	resp := bazilfuse.MkdirResponse{}
	convertChildInodeEntry(&o.Entry, &resp.LookupResponse)

	o.commonOp.logger.Printf("Responding: %v", &resp)
	o.r.(*bazilfuse.MkdirRequest).Respond(&resp)
}

// Create a file inode and open it.
//
// The kernel sends this when the user asks to open a file with the O_CREAT
// flag and the kernel has observed that the file doesn't exist. (See for
// example lookup_open, http://goo.gl/PlqE9d).
//
// However it's impossible to tell for sure that all kernels make this check
// in all cases and the official fuse documentation is less than encouraging
// (" the file does not exist, first create it with the specified mode, and
// then open it"). Therefore file systems would be smart to be paranoid and
// check themselves, returning EEXIST when the file already exists. This of
// course particularly applies to file systems that are volatile from the
// kernel's point of view.
type CreateFileOp struct {
	commonOp

	// The ID of parent directory inode within which to create the child file.
	Parent InodeID

	// The name of the child to create, and the mode with which to create it.
	Name string
	Mode os.FileMode

	// Flags for the open operation.
	Flags bazilfuse.OpenFlags

	// Set by the file system: information about the inode that was created.
	//
	// The lookup count for the inode is implicitly incremented. See notes on
	// ForgetInodeOp for more information.
	Entry ChildInodeEntry

	// Set by the file system: an opaque ID that will be echoed in follow-up
	// calls for this file using the same struct file in the kernel. In practice
	// this usually means follow-up calls using the file descriptor returned by
	// open(2).
	//
	// The handle may be supplied in future ops like ReadFileOp that contain a
	// file handle. The file system must ensure this ID remains valid until a
	// later call to ReleaseFileHandle.
	Handle HandleID
}

func (o *CreateFileOp) Respond(err error) {
	if err != nil {
		o.commonOp.respondErr(err)
		return
	}

	resp := bazilfuse.CreateResponse{
		OpenResponse: bazilfuse.OpenResponse{
			Handle: bazilfuse.HandleID(o.Handle),
		},
	}
	convertChildInodeEntry(&o.Entry, &resp.LookupResponse)

	o.commonOp.logger.Printf("Responding: %v", &resp)
	o.r.(*bazilfuse.CreateRequest).Respond(&resp)
}

////////////////////////////////////////////////////////////////////////
// Unlinking
////////////////////////////////////////////////////////////////////////

// Unlink a directory from its parent. Because directories cannot have a link
// count above one, this means the directory inode should be deleted as well
// once the kernel sends ForgetInodeOp.
//
// The file system is responsible for checking that the directory is empty.
//
// Sample implementation in ext2: ext2_rmdir (http://goo.gl/B9QmFf)
type RmDirOp struct {
	commonOp

	// The ID of parent directory inode, and the name of the directory being
	// removed within it.
	Parent InodeID
	Name   string
}

func (o *RmDirOp) Respond(err error) {
	if err != nil {
		o.commonOp.respondErr(err)
		return
	}

	o.commonOp.logger.Printf("Responding OK to RmDirOp")
	o.r.(*bazilfuse.RemoveRequest).Respond()
}

// Unlink a file from its parent. If this brings the inode's link count to
// zero, the inode should be deleted once the kernel sends ForgetInodeOp. It
// may still be referenced before then if a user still has the file open.
//
// Sample implementation in ext2: ext2_unlink (http://goo.gl/hY6r6C)
type UnlinkOp struct {
	commonOp

	// The ID of parent directory inode, and the name of the file being removed
	// within it.
	Parent InodeID
	Name   string
}

func (o *UnlinkOp) Respond(err error) {
	if err != nil {
		o.commonOp.respondErr(err)
		return
	}

	o.commonOp.logger.Printf("Responding OK to UnlinkOp")
	o.r.(*bazilfuse.RemoveRequest).Respond()
}

////////////////////////////////////////////////////////////////////////
// Directory handles
////////////////////////////////////////////////////////////////////////

// Open a directory inode.
//
// On Linux the sends this when setting up a struct file for a particular inode
// with type directory, usually in response to an open(2) call from a
// user-space process. On OS X it may not be sent for every open(2) (cf.
// https://github.com/osxfuse/osxfuse/issues/199).
type OpenDirOp struct {
	commonOp

	// The ID of the inode to be opened.
	Inode InodeID

	// Mode and options flags.
	Flags bazilfuse.OpenFlags

	// Set by the file system: an opaque ID that will be echoed in follow-up
	// calls for this directory using the same struct file in the kernel. In
	// practice this usually means follow-up calls using the file descriptor
	// returned by open(2).
	//
	// The handle may be supplied in future ops like ReadDirOp that contain a
	// directory handle. The file system must ensure this ID remains valid until
	// a later call to ReleaseDirHandle.
	Handle HandleID
}

func (o *OpenDirOp) Respond(err error) {
	if err != nil {
		o.commonOp.respondErr(err)
		return
	}

	resp := bazilfuse.OpenResponse{
		Handle: bazilfuse.HandleID(o.Handle),
	}

	o.commonOp.logger.Printf("Responding: %v", &resp)
	o.r.(*bazilfuse.OpenRequest).Respond(&resp)
}

// Read entries from a directory previously opened with OpenDir.
type ReadDirOp struct {
	commonOp

	// The directory inode that we are reading, and the handle previously
	// returned by OpenDir when opening that inode.
	Inode  InodeID
	Handle HandleID

	// The offset within the directory at which to read.
	//
	// Warning: this field is not necessarily a count of bytes. Its legal values
	// are defined by the results returned in ReadDirResponse. See the notes
	// below and the notes on that struct.
	//
	// In the Linux kernel this ultimately comes from file::f_pos, which starts
	// at zero and is set by llseek and by the final consumed result returned by
	// each call to ReadDir:
	//
	//  *  (http://goo.gl/2nWJPL) iterate_dir, which is called by getdents(2) and
	//     readdir(2), sets dir_context::pos to file::f_pos before calling
	//     f_op->iterate, and then does the opposite assignment afterward.
	//
	//  *  (http://goo.gl/rTQVSL) fuse_readdir, which implements iterate for fuse
	//     directories, passes dir_context::pos as the offset to fuse_read_fill,
	//     which passes it on to user-space. fuse_readdir later calls
	//     parse_dirfile with the same context.
	//
	//  *  (http://goo.gl/vU5ukv) For each returned result (except perhaps the
	//     last, which may be truncated by the page boundary), parse_dirfile
	//     updates dir_context::pos with fuse_dirent::off.
	//
	// It is affected by the Posix directory stream interfaces in the following
	// manner:
	//
	//  *  (http://goo.gl/fQhbyn, http://goo.gl/ns1kDF) opendir initially causes
	//     filepos to be set to zero.
	//
	//  *  (http://goo.gl/ezNKyR, http://goo.gl/xOmDv0) readdir allows the user
	//     to iterate through the directory one entry at a time. As each entry is
	//     consumed, its d_off field is stored in __dirstream::filepos.
	//
	//  *  (http://goo.gl/WEOXG8, http://goo.gl/rjSXl3) telldir allows the user
	//     to obtain the d_off field from the most recently returned entry.
	//
	//  *  (http://goo.gl/WG3nDZ, http://goo.gl/Lp0U6W) seekdir allows the user
	//     to seek backward to an offset previously returned by telldir. It
	//     stores the new offset in filepos, and calls llseek to update the
	//     kernel's struct file.
	//
	//  *  (http://goo.gl/gONQhz, http://goo.gl/VlrQkc) rewinddir allows the user
	//     to go back to the beginning of the directory, obtaining a fresh view.
	//     It updates filepos and calls llseek to update the kernel's struct
	//     file.
	//
	// Unfortunately, FUSE offers no way to intercept seeks
	// (http://goo.gl/H6gEXa), so there is no way to cause seekdir or rewinddir
	// to fail. Additionally, there is no way to distinguish an explicit
	// rewinddir followed by readdir from the initial readdir, or a rewinddir
	// from a seekdir to the value returned by telldir just after opendir.
	//
	// Luckily, Posix is vague about what the user will see if they seek
	// backwards, and requires the user not to seek to an old offset after a
	// rewind. The only requirement on freshness is that rewinddir results in
	// something that looks like a newly-opened directory. So FUSE file systems
	// may e.g. cache an entire fresh listing for each ReadDir with a zero
	// offset, and return array offsets into that cached listing.
	Offset DirOffset

	// The maximum number of bytes to return in ReadDirResponse.Data. A smaller
	// number is acceptable.
	Size int

	// Set by the file system: a buffer consisting of a sequence of FUSE
	// directory entries in the format generated by fuse_add_direntry
	// (http://goo.gl/qCcHCV), which is consumed by parse_dirfile
	// (http://goo.gl/2WUmD2). Use fuseutil.AppendDirent to generate this data.
	//
	// The buffer must not exceed the length specified in ReadDirRequest.Size. It
	// is okay for the final entry to be truncated; parse_dirfile copes with this
	// by ignoring the partial record.
	//
	// Each entry returned exposes a directory offset to the user that may later
	// show up in ReadDirRequest.Offset. See notes on that field for more
	// information.
	//
	// An empty buffer indicates the end of the directory has been reached.
	Data []byte
}

func (o *ReadDirOp) Respond(err error) {
	if err != nil {
		o.commonOp.respondErr(err)
		return
	}

	resp := bazilfuse.ReadResponse{
		Data: o.Data,
	}

	o.commonOp.logger.Printf("Responding: %v", &resp)
	o.r.(*bazilfuse.ReadRequest).Respond(&resp)
}

// Release a previously-minted directory handle. The kernel sends this when
// there are no more references to an open directory: all file descriptors are
// closed and all memory mappings are unmapped.
//
// The kernel guarantees that the handle ID will not be used in further ops
// sent to the file system (unless it is reissued by the file system).
//
// Errors from this op are ignored by the kernel (cf. http://goo.gl/RL38Do).
type ReleaseDirHandleOp struct {
	commonOp

	// The handle ID to be released. The kernel guarantees that this ID will not
	// be used in further calls to the file system (unless it is reissued by the
	// file system).
	Handle HandleID
}

func (o *ReleaseDirHandleOp) Respond(err error) {
	if err != nil {
		o.commonOp.respondErr(err)
		return
	}

	o.commonOp.logger.Printf("Responding OK to ReleaseDirHandleOp")
	o.r.(*bazilfuse.ReleaseRequest).Respond()
}

////////////////////////////////////////////////////////////////////////
// File handles
////////////////////////////////////////////////////////////////////////

// Open a file inode.
//
// On Linux the sends this when setting up a struct file for a particular inode
// with type file, usually in response to an open(2) call from a user-space
// process. On OS X it may not be sent for every open(2)
// (cf.https://github.com/osxfuse/osxfuse/issues/199).
type OpenFileOp struct {
	commonOp

	// The ID of the inode to be opened.
	Inode InodeID

	// Mode and options flags.
	Flags bazilfuse.OpenFlags

	// An opaque ID that will be echoed in follow-up calls for this file using
	// the same struct file in the kernel. In practice this usually means
	// follow-up calls using the file descriptor returned by open(2).
	//
	// The handle may be supplied in future ops like ReadFileOp that contain a
	// file handle. The file system must ensure this ID remains valid until a
	// later call to ReleaseFileHandle.
	Handle HandleID
}

func (o *OpenFileOp) Respond(err error) {
	if err != nil {
		o.commonOp.respondErr(err)
		return
	}

	resp := bazilfuse.OpenResponse{
		Handle: bazilfuse.HandleID(o.Handle),
	}

	o.commonOp.logger.Printf("Responding: %v", &resp)
	o.r.(*bazilfuse.OpenRequest).Respond(&resp)
}

// Read data from a file previously opened with CreateFile or OpenFile.
//
// Note that this op is not sent for every call to read(2) by the end user;
// some reads may be served by the page cache. See notes on WriteFileOp for
// more.
type ReadFileOp struct {
	commonOp

	// The file inode that we are reading, and the handle previously returned by
	// CreateFile or OpenFile when opening that inode.
	Inode  InodeID
	Handle HandleID

	// The range of the file to read.
	//
	// The FUSE documentation requires that exactly the number of bytes be
	// returned, except in the case of EOF or error (http://goo.gl/ZgfBkF). This
	// appears to be because it uses file mmapping machinery
	// (http://goo.gl/SGxnaN) to read a page at a time. It appears to understand
	// where EOF is by checking the inode size (http://goo.gl/0BkqKD), returned
	// by a previous call to LookUpInode, GetInodeAttributes, etc.
	Offset int64
	Size   int

	// Set by the file system: the data read. If this is less than the requested
	// size, it indicates EOF. An error should not be returned in this case.
	Data []byte
}

func (o *ReadFileOp) Respond(err error) {
	if err != nil {
		o.commonOp.respondErr(err)
		return
	}

	resp := bazilfuse.ReadResponse{
		Data: o.Data,
	}

	o.commonOp.logger.Printf("Responding: %v", &resp)
	o.r.(*bazilfuse.ReadRequest).Respond(&resp)
}

// Write data to a file previously opened with CreateFile or OpenFile.
//
// When the user writes data using write(2), the write goes into the page
// cache and the page is marked dirty. Later the kernel may write back the
// page via the FUSE VFS layer, causing this op to be sent:
//
//  *  The kernel calls address_space_operations::writepage when a dirty page
//     needs to be written to backing store (cf. http://goo.gl/Ezbewg). Fuse
//     sets this to fuse_writepage (cf. http://goo.gl/IeNvLT).
//
//  *  (http://goo.gl/Eestuy) fuse_writepage calls fuse_writepage_locked.
//
//  *  (http://goo.gl/RqYIxY) fuse_writepage_locked makes a write request to
//     the userspace server.
//
// Note that writes *will* be received before a FlushOp when closing the file
// descriptor to which they were written:
//
//  *  (http://goo.gl/PheZjf) fuse_flush calls write_inode_now, which appears
//     to start a writeback in the background (it talks about a "flusher
//     thread").
//
//  *  (http://goo.gl/1IiepM) fuse_flush then calls fuse_sync_writes, which
//     "[waits] for all pending writepages on the inode to finish".
//
//  *  (http://goo.gl/zzvxWv) Only then does fuse_flush finally send the
//     flush request.
//
type WriteFileOp struct {
	commonOp

	// The file inode that we are modifying, and the handle previously returned
	// by CreateFile or OpenFile when opening that inode.
	Inode  InodeID
	Handle HandleID

	// The offset at which to write the data below.
	//
	// The man page for pwrite(2) implies that aside from changing the file
	// handle's offset, using pwrite is equivalent to using lseek(2) and then
	// write(2). The man page for lseek(2) says the following:
	//
	// "The lseek() function allows the file offset to be set beyond the end of
	// the file (but this does not change the size of the file). If data is later
	// written at this point, subsequent reads of the data in the gap (a "hole")
	// return null bytes (aq\0aq) until data is actually written into the gap."
	//
	// It is therefore reasonable to assume that the kernel is looking for
	// the following semantics:
	//
	// *   If the offset is less than or equal to the current size, extend the
	//     file as necessary to fit any data that goes past the end of the file.
	//
	// *   If the offset is greater than the current size, extend the file
	//     with null bytes until it is not, then do the above.
	//
	Offset int64

	// The data to write.
	//
	// The FUSE documentation requires that exactly the number of bytes supplied
	// be written, except on error (http://goo.gl/KUpwwn). This appears to be
	// because it uses file mmapping machinery (http://goo.gl/SGxnaN) to write a
	// page at a time.
	Data []byte
}

func (o *WriteFileOp) Respond(err error) {
	if err != nil {
		o.commonOp.respondErr(err)
		return
	}

	resp := bazilfuse.WriteResponse{
		Size: len(o.Data),
	}

	o.commonOp.logger.Printf("Responding: %v", &resp)
	o.r.(*bazilfuse.WriteRequest).Respond(&resp)
}

// Synchronize the current contents of an open file to storage.
//
// vfs.txt documents this as being called for by the fsync(2) system call
// (cf. http://goo.gl/j9X8nB). Code walk for that case:
//
//  *  (http://goo.gl/IQkWZa) sys_fsync calls do_fsync, calls vfs_fsync, calls
//     vfs_fsync_range.
//
//  *  (http://goo.gl/5L2SMy) vfs_fsync_range calls f_op->fsync.
//
// Note that this is also sent by fdatasync(2) (cf. http://goo.gl/01R7rF), and
// may be sent for msync(2) with the MS_SYNC flag (see the notes on
// FlushFileOp).
//
// See also: FlushFileOp, which may perform a similar function when closing a
// file (but which is not used in "real" file systems).
type SyncFileOp struct {
	commonOp

	// The file and handle being sync'd.
	Inode  InodeID
	Handle HandleID
}

func (o *SyncFileOp) Respond(err error) {
	if err != nil {
		o.commonOp.respondErr(err)
		return
	}

	o.commonOp.logger.Printf("Responding OK to SyncFileOp")
	o.r.(*bazilfuse.FsyncRequest).Respond()
}

// Flush the current state of an open file to storage upon closing a file
// descriptor.
//
// vfs.txt documents this as being sent for each close(2) system call (cf.
// http://goo.gl/FSkbrq). Code walk for that case:
//
//  *  (http://goo.gl/e3lv0e) sys_close calls __close_fd, calls filp_close.
//  *  (http://goo.gl/nI8fxD) filp_close calls f_op->flush (fuse_flush).
//
// But note that this is also sent in other contexts where a file descriptor is
// closed, such as dup2(2) (cf. http://goo.gl/NQDvFS). In the case of close(2),
// a flush error is returned to the user. For dup2(2), it is not.
//
// One potentially significant case where this may not be sent is mmap'd files,
// where the behavior is complicated:
//
//  *  munmap(2) does not cause flushes (cf. http://goo.gl/j8B9g0).
//
//  *  On OS X, if a user modifies a mapped file via the mapping before
//     closing the file with close(2), the WriteFileOps for the modifications
//     may not be received before the FlushFileOp for the close(2) (cf.
//     http://goo.gl/kVmNcx).
//
//  *  However, even on OS X you can arrange for writes via a mapping to be
//     flushed by calling msync(2) followed by close(2). On OS X msync(2)
//     will cause a WriteFile to go through and close(2) will cause a
//     FlushFile as usual (cf. http://goo.gl/kVmNcx). On Linux, msync(2) does
//     nothing unless you set the MS_SYNC flag, in which case it causes a
//     SyncFile (cf. http://goo.gl/P3mErk).
//
// In summary: if you make data durable in both FlushFile and SyncFile, then
// your users can get safe behavior from mapped files by calling msync(2)
// with MS_SYNC, followed by munmap(2), followed by close(2). On Linux, the
// msync(2) appears to be optional because close(2) implies dirty page
// writeback (cf. http://goo.gl/HyzLTT).
//
// Because of cases like dup2(2), FlushFileOps are not necessarily one to one
// with OpenFileOps. They should not be used for reference counting, and the
// handle must remain valid even after the flush op is received (use
// ReleaseFileHandleOp for disposing of it).
//
// Typical "real" file systems do not implement this, presumably relying on
// the kernel to write out the page cache to the block device eventually.
// They can get away with this because a later open(2) will see the same
// data. A file system that writes to remote storage however probably wants
// to at least schedule a real flush, and maybe do it immediately in order to
// return any errors that occur.
type FlushFileOp struct {
	commonOp

	// The file and handle being flushed.
	Inode  InodeID
	Handle HandleID
}

func (o *FlushFileOp) Respond(err error) {
	if err != nil {
		o.commonOp.respondErr(err)
		return
	}

	o.commonOp.logger.Printf("Responding OK to FlushFileOp")
	o.r.(*bazilfuse.FlushRequest).Respond()
}

// Release a previously-minted file handle. The kernel calls this when there
// are no more references to an open file: all file descriptors are closed
// and all memory mappings are unmapped.
//
// The kernel guarantees that the handle ID will not be used in further calls
// to the file system (unless it is reissued by the file system).
//
// Errors from this op are ignored by the kernel (cf. http://goo.gl/RL38Do).
type ReleaseFileHandleOp struct {
	commonOp

	// The handle ID to be released. The kernel guarantees that this ID will not
	// be used in further calls to the file system (unless it is reissued by the
	// file system).
	Handle HandleID
}

func (o *ReleaseFileHandleOp) Respond(err error) {
	if err != nil {
		o.commonOp.respondErr(err)
		return
	}

	o.commonOp.logger.Printf("Responding OK to ReleaseFileHandleOp")
	o.r.(*bazilfuse.ReleaseRequest).Respond()
}