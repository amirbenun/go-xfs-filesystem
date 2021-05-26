package xfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"unsafe"

	"golang.org/x/xerrors"
)

const (
	BMBT_EXNTFLAG_BITLEN = 1
	INODEV3_SIZE         = 176
	INODE_SIZE           = 96

	XFS_DIR2_DATA_FD_COUNT  = 3
	XFS_DIR2_DATA_FREE_TAG  = 0xffff
	XFS_DIR2_DATA_ALIGN_LOG = 3

	LEAF_ENTRY_SIZE = 8

	// Block Directory Magic number
	XDB3 = 0x58444233

	// Leaf Directory Magic number
	XDD3 = 0x58444433
)

const (
	XFS_DIR2_DATA_SPACE = iota
	XFS_DIR2_LEAF_SPACE
	XFS_DIR2_FREE_SPACE
)

const (
	// typedef enum xfs_dinode_fmt
	XFS_DINODE_FMT_DEV = iota
	XFS_DINODE_FMT_LOCAL
	XFS_DINODE_FMT_EXTENTS
	XFS_DINODE_FMT_BTREE
	XFS_DINODE_FMT_UUID
	XFS_DINODE_FMT_RMAP
)

var (
	XFS_DIR2_SPACE_SIZE  = 1 << (32 + XFS_DIR2_DATA_ALIGN_LOG)
	XFS_DIR2_DATA_OFFSET = XFS_DIR2_DATA_SPACE * XFS_DIR2_SPACE_SIZE
	XFS_DIR2_LEAF_OFFSET = XFS_DIR2_LEAF_SPACE * XFS_DIR2_SPACE_SIZE
	XFS_DIR2_FREE_OFFSET = XFS_DIR2_FREE_SPACE * XFS_DIR2_SPACE_SIZE
)

func (xfs *FileSystem) ParseInode(ino uint64) (*Inode, error) {
	xfs.seekInode(ino)
	r := io.LimitReader(xfs.file, int64(xfs.PrimaryAG.SuperBlock.Inodesize))

	inode := Inode{}

	if err := binary.Read(r, binary.BigEndian, &inode.inodeCore); err != nil {
		return nil, xerrors.Errorf("failed to read InodeCore: %w", err)
	}

	if !inode.inodeCore.isSupported() {
		panic(fmt.Sprintf("not support inode version %+v", inode))
	}

	switch inode.inodeCore.Format {
	case XFS_DINODE_FMT_DEV:
		inode.device = &Device{}
	case XFS_DINODE_FMT_LOCAL:
		if inode.inodeCore.IsDir() {
			inode.directoryLocal = &DirectoryLocal{}
			if err := binary.Read(r, binary.BigEndian, &inode.directoryLocal.dir2SfHdr); err != nil {
				return nil, xerrors.Errorf("failed to read XFS_DINODE_FMT_LOCAL directory error: %w", err)
			}
			if inode.directoryLocal.dir2SfHdr.I8Count != 0 {
				panic("header inode number 8 byte panic")
			}
			for i := 0; i < int(inode.directoryLocal.dir2SfHdr.Count); i++ {
				entry, err := parseEntry(r)
				if err != nil {
					log.Fatal(err)
				}
				inode.directoryLocal.entries = append(inode.directoryLocal.entries, *entry)
			}
		} else if inode.inodeCore.IsSymlink() {
			inode.symlinkString = &SymlinkString{}
			buf := make([]byte, inode.inodeCore.Size)
			_, err := r.Read(buf)
			if err != nil {
				return nil, xerrors.Errorf("failed to read XFS_DINODE_FMT_LOCAL symlink error: %w", err)
			}
			inode.symlinkString.Name = string(buf)
		} else {
			panic("not support XFS_DINODE_FMT_LOCAL")
		}
	case XFS_DINODE_FMT_EXTENTS:
		if inode.inodeCore.IsDir() {
			inode.directoryExtents = &DirectoryExtents{}
			for i := uint32(0); i < inode.inodeCore.Nextents; i++ {
				var bmbtRec BmbtRec
				if err := binary.Read(r, binary.BigEndian, &bmbtRec); err != nil {
					return nil, xerrors.Errorf("failed to read xfs_bmbt_irec error: %w", err)
				}
				inode.directoryExtents.bmbtRecs = append(inode.directoryExtents.bmbtRecs, bmbtRec)
			}
		} else if inode.inodeCore.IsRegular() {
			inode.regularExtent = &RegularExtent{}
			for i := uint32(0); i < inode.inodeCore.Nextents; i++ {
				var bmbtRec BmbtRec
				if err := binary.Read(r, binary.BigEndian, &bmbtRec); err != nil {
					return nil, xerrors.Errorf("failed to read xfs_bmbt_irec error: %w", err)
				}
				inode.regularExtent.bmbtRecs = append(inode.regularExtent.bmbtRecs, bmbtRec)
			}
		} else if inode.inodeCore.IsSymlink() {
			panic("not support XFS_DINODE_FMT_EXTENTS isSymlink")
		} else {
			panic("not support XFS_DINODE_FMT_EXTENTS")
		}
	case XFS_DINODE_FMT_BTREE:
		panic("not support XFS_DINODE_FMT_BTREE")
	case XFS_DINODE_FMT_UUID:
		panic("not support XFS_DINODE_FMT_UUID")
	case XFS_DINODE_FMT_RMAP:
		panic("not support XFS_DINODE_FMT_RMAP")
	default:
		panic("not support")
	}

	// TODO: support extend attribute fork , see. Chapter 19 Extended Attributes
	// if inode.inodeCore.Forkoff != 0 {
	// 	fmt.Printf("%+v\n", inode.inodeCore)
	// 	panic("has extend attribute fork")
	// }

	// TODO: Need parse extended attribute fork.
	ioutil.ReadAll(r)
	return &inode, nil
}

func (i *Inode) AttributeOffset() uint32 {
	return uint32(i.inodeCore.Forkoff)*8 + INODEV3_SIZE
}

func (i *Inode) String() string {
	var s string
	s = fmt.Sprintf("%+v\n", i.inodeCore)

	if i.directoryLocal != nil {
		s = s + fmt.Sprintf("%+v\n", i.directoryLocal)
	}
	if i.directoryExtents != nil {
		s = s + fmt.Sprintf("%+v\n", i.directoryExtents)
		for i, b := range i.directoryExtents.bmbtRecs {
			s = s + fmt.Sprintf("%d: %+v\n", i, b.Unpack())
		}
	}
	if i.regularExtent != nil {
		s = s + fmt.Sprintf("%+v\n", i.regularExtent)
		for i, b := range i.regularExtent.bmbtRecs {
			s = s + fmt.Sprintf("%d: %+v\n", i, b.Unpack())
		}
	}

	if i.symlinkString != nil {
		s = s + fmt.Sprintf("%+v\n", i.symlinkString)
	}

	if i.device != nil {
		s = s + "DEVICE\n"
	}

	return s
}

var UnsupportedDir2BlockHeaderErr = xerrors.New("unsupported block")

const (
	FreeTag = 0xffff
)

func (xfs *FileSystem) parseXDB3Block(r io.Reader) ([]Dir2DataEntry, error) {
	buf, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, xerrors.Errorf("failed to read XDB3 block reader: %w", err)
	}
	var tail Dir2BlockTail
	tailReader := bytes.NewReader(buf[len(buf)-int(unsafe.Sizeof(tail)):])
	if err := binary.Read(tailReader, binary.BigEndian, &tail); err != nil {
		return nil, xerrors.Errorf("failed to read tail binary: %w", err)
	}
	reader := bytes.NewReader(buf[:uint32(len(buf))-((tail.Count*LEAF_ENTRY_SIZE)+uint32(unsafe.Sizeof(tail)))])

	entries := []Dir2DataEntry{}
	for {
		entry := Dir2DataEntry{}

		ino := make([]byte, unsafe.Sizeof(entry.Inumber))
		_, err := reader.Read(ino)
		if err != nil {
			if err == io.EOF {
				return entries, nil
			}
			return nil, xerrors.Errorf("failed to read inumber: %w", err)
		}

		if err := binary.Read(bytes.NewReader(ino), binary.BigEndian, &entry.Inumber); err != nil {
			return nil, xerrors.Errorf("failed to read inumber binary: %w", err)
		}
		if (entry.Inumber >> 48) == FreeTag {
			freeLen := (entry.Inumber >> 32) & Mask64Lo(16)
			if freeLen != 8 {
				// Read FreeTag tail
				_, err := reader.Read(make([]byte, freeLen-0x08))
				if err != nil {
					return nil, xerrors.Errorf("failed to read unused padding: %w", err)
				}
			}
			continue
		}
		if err := binary.Read(reader, binary.BigEndian, &entry.Namelen); err != nil {

			return nil, xerrors.Errorf("failed to read name length: %w", err)
		}
		nameBuf := make([]byte, entry.Namelen)
		n, err := reader.Read(nameBuf)
		if err != nil {
			return nil, xerrors.Errorf("failed to read name: %w", err)
		}
		if n != int(entry.Namelen) {
			return nil, xerrors.Errorf("failed to read name: expected namelen(%d) actual(%d)", entry.Namelen, n)
		}
		entry.EntryName = string(nameBuf)

		if err := binary.Read(reader, binary.BigEndian, &entry.Filetype); err != nil {
			return nil, xerrors.Errorf("failed to read file type: %w", err)
		}

		// 12 = Inumber + Namelen + Filetype + Tag
		// 8  = Alignment
		align := (int(unsafe.Sizeof(entry.Inumber)) +
			int(unsafe.Sizeof(entry.Namelen)) +
			int(unsafe.Sizeof(entry.Filetype)) +
			int(unsafe.Sizeof(entry.Tag)) + n) % 8
		if align != 0 {
			n, err = reader.Read(make([]byte, 8-align))
			if err != nil {
				return nil, xerrors.Errorf("failed to read alignment: %w", err)
			}
			if n != int(8-align) {
				return nil, xerrors.Errorf("failed to read alignment: expected namelen(%d) actual(%d)", entry.Namelen, n)
			}
		}

		if err := binary.Read(reader, binary.BigEndian, &entry.Tag); err != nil {
			return nil, xerrors.Errorf("failed to read tag: %w", err)
		}
		entries = append(entries, entry)
	}
}

func (xfs *FileSystem) parseXDD3Block(r io.Reader) ([]Dir2DataEntry, error) {
	entries := []Dir2DataEntry{}
	for {
		entry := Dir2DataEntry{}

		ino := make([]byte, unsafe.Sizeof(entry.Inumber))
		_, err := r.Read(ino)
		if err != nil {
			if err == io.EOF {
				return entries, nil
			}
			return nil, xerrors.Errorf("failed to read inumber: %w", err)
		}

		if err := binary.Read(bytes.NewReader(ino), binary.BigEndian, &entry.Inumber); err != nil {
			return nil, xerrors.Errorf("failed to read inumber binary: %w", err)
		}
		if (entry.Inumber >> 48) == FreeTag {
			freeLen := (entry.Inumber >> 32) & Mask64Lo(16)
			if freeLen != 8 {
				// Read FreeTag tail
				_, err := r.Read(make([]byte, freeLen-0x08))
				if err != nil {
					return nil, xerrors.Errorf("failed to read unused padding: %w", err)
				}
			}

			continue
		}
		if err := binary.Read(r, binary.BigEndian, &entry.Namelen); err != nil {
			return nil, xerrors.Errorf("failed to read name length: %w", err)
		}
		nameBuf := make([]byte, entry.Namelen)
		n, err := r.Read(nameBuf)
		if err != nil {
			return nil, xerrors.Errorf("failed to read name: %w", err)
		}
		if n != int(entry.Namelen) {
			return nil, xerrors.Errorf("failed to read name: expected namelen(%d) actual(%d)", entry.Namelen, n)
		}
		entry.EntryName = string(nameBuf)

		if err := binary.Read(r, binary.BigEndian, &entry.Filetype); err != nil {
			return nil, xerrors.Errorf("failed to read file type: %w", err)
		}

		// 12 = Inumber + Namelen + Filetype + Tag
		// 8  = Alignment
		align := (int(unsafe.Sizeof(entry.Inumber)) +
			int(unsafe.Sizeof(entry.Namelen)) +
			int(unsafe.Sizeof(entry.Filetype)) +
			int(unsafe.Sizeof(entry.Tag)) + n) % 8
		if align != 0 {
			n, err = r.Read(make([]byte, 8-align))
			if err != nil {
				return nil, xerrors.Errorf("failed to read alignment: %w", err)
			}
			if n != int(8-align) {
				return nil, xerrors.Errorf("failed to read alignment: expected namelen(%d) actual(%d)", entry.Namelen, n)
			}
		}

		if err := binary.Read(r, binary.BigEndian, &entry.Tag); err != nil {
			return nil, xerrors.Errorf("failed to read tag: %w", err)
		}
		entries = append(entries, entry)
	}
}

func (xfs *FileSystem) parseDir2Block(bmbtIrec BmbtIrec) (*Dir2Block, error) {
	block := Dir2Block{}
	if bmbtIrec.StartBlock >= uint64(XFS_DIR2_LEAF_OFFSET) {
		// Skip Leaf and Free node
		return &block, nil
	}

	physicalBlockOffset := xfs.PrimaryAG.SuperBlock.BlockToPhysicalOffset(bmbtIrec.StartBlock)
	xfs.seekBlock(physicalBlockOffset)
	r := io.LimitReader(xfs.file, int64(xfs.PrimaryAG.SuperBlock.BlockSize*uint32(bmbtIrec.BlockCount)))
	if err := binary.Read(r, binary.BigEndian, &block.Header); err != nil {
		return nil, xerrors.Errorf("failed to parse block header error: %w", err)
	}

	var err error
	switch block.Header.Magic {
	case XDD3:
		block.Entries, err = xfs.parseXDD3Block(r)
		if err != nil {
			return nil, xerrors.Errorf("failed to parse XDD3 block: %w", err)
		}
	case XDB3:
		block.Entries, err = xfs.parseXDB3Block(r)
		if err != nil {
			return nil, xerrors.Errorf("failed to parse XDB3 block: %w", err)
		}
	default:
		return nil, xerrors.Errorf("failed to parse header error magic: %v: %w", block.Header.Magic, UnsupportedDir2BlockHeaderErr)
	}

	return &block, nil
}

func parseEntry(r io.Reader) (*Dir2SfEntry, error) {
	var entry Dir2SfEntry
	if err := binary.Read(r, binary.BigEndian, &entry.Namelen); err != nil {
		return nil, err
	}

	if err := binary.Read(r, binary.BigEndian, &entry.Offset); err != nil {
		return nil, err
	}
	buf := make([]byte, entry.Namelen)
	i, err := r.Read(buf)
	if err != nil {
		return nil, err
	}
	if i != int(entry.Namelen) {
		return nil, errors.New("")
	}
	entry.EntryName = string(buf)
	if err := binary.Read(r, binary.BigEndian, &entry.Filetype); err != nil {
		return nil, err
	}

	if err := binary.Read(r, binary.BigEndian, &entry.Inumber); err != nil {
		return nil, err
	}

	return &entry, nil
}

func ParseBlockDirectories(reader io.Reader) {

}

type Inode struct {
	inodeCore InodeCore
	// Device
	device *Device

	// S_IFDIR
	directoryLocal   *DirectoryLocal
	directoryExtents *DirectoryExtents

	// S_IFREG
	regularExtent *RegularExtent

	// S_IFLNK
	symlinkString *SymlinkString
}

type RegularExtent struct {
	bmbtRecs []BmbtRec
}

type DirectoryExtents struct {
	bmbtRecs []BmbtRec
}

type DirectoryLocal struct {
	dir2SfHdr Dir2SfHdr
	entries   []Dir2SfEntry
}

// https://github.com/torvalds/linux/blob/d2b6f8a179194de0ffc4886ffc2c4358d86047b8/fs/xfs/libxfs/xfs_format.h#L1787
type BmbtRec struct {
	L0 uint64
	L1 uint64
}

// https://github.com/torvalds/linux/blob/d2b6f8a179194de0ffc4886ffc2c4358d86047b8/fs/xfs/libxfs/xfs_bmap_btree.c#L60
func (b BmbtRec) Unpack() BmbtIrec {
	return BmbtIrec{
		StartOff:   (b.L0 & Mask64Lo(64-BMBT_EXNTFLAG_BITLEN)) >> 9,
		StartBlock: ((b.L0 & Mask64Lo(9)) << 43) | (b.L1 >> 21),
		BlockCount: (b.L1 & Mask64Lo(21)),
	}
}

func Mask64Lo(n int) uint64 {
	return (1 << n) - 1
}

// https://github.com/torvalds/linux/blob/5bfc75d92efd494db37f5c4c173d3639d4772966/fs/xfs/libxfs/xfs_types.h#L162
type BmbtIrec struct {
	StartOff   uint64
	StartBlock uint64
	BlockCount uint64
	State      uint8
}

// https://github.com/torvalds/linux/blob/5bfc75d92efd494db37f5c4c173d3639d4772966/fs/xfs/libxfs/xfs_da_format.h#L203-L207
type Dir2SfHdr struct {
	Count   uint8
	I8Count uint8
	Parent  uint32
}

type Dir2Block struct {
	Header  Dir3DataHdr
	Entries []Dir2DataEntry

	UnusedEntries []Dir2DataUnused
	Leafs         []Dir2LeafEntry
	Tail          Dir2BlockTail
}

type Dir2BlockTail struct {
	Count uint32
	Stale uint32
}

type Dir2LeafEntry struct {
	Hashval uint32
	Address uint32
}

// https://github.com/torvalds/linux/blob/5bfc75d92efd494db37f5c4c173d3639d4772966/fs/xfs/libxfs/xfs_da_format.h#L320-L324
type Dir3DataHdr struct {
	Dir3BlkHdr
	Frees   [XFS_DIR2_DATA_FD_COUNT]Dir2DataFree
	Padding uint32
}

// https://github.com/torvalds/linux/blob/5bfc75d92efd494db37f5c4c173d3639d4772966/fs/xfs/libxfs/xfs_da_format.h#L311-L318
type Dir3BlkHdr struct {
	Magic    uint32
	CRC      uint32
	BlockNo  uint64
	Lsn      uint64
	MetaUUID [16]byte
	Owner    uint64
}

// https://github.com/torvalds/linux/blob/5bfc75d92efd494db37f5c4c173d3639d4772966/fs/xfs/libxfs/xfs_da_format.h#L353-L358
type Dir2DataUnused struct {
	Freetag uint16
	Length  uint16
	/* variable offset */
	Tag uint16
}

type Dir2DataFree struct {
	Offset uint16
	Length uint16
}

func (e Dir2SfEntry) FileType() uint8 {
	return e.Filetype
}
func (e Dir2DataEntry) FileType() uint8 {
	return e.Filetype
}
func (e Dir2SfEntry) Name() string {
	return e.EntryName
}
func (e Dir2DataEntry) Name() string {
	return e.EntryName
}
func (e Dir2SfEntry) InodeNumber() uint64 {
	return uint64(e.Inumber)
}
func (e Dir2DataEntry) InodeNumber() uint64 {
	return e.Inumber
}

var _ Entry = &Dir2DataEntry{}
var _ Entry = &Dir2SfEntry{}

type Entry interface {
	Name() string
	FileType() uint8
	InodeNumber() uint64
}

// https://github.com/torvalds/linux/blob/5bfc75d92efd494db37f5c4c173d3639d4772966/fs/xfs/libxfs/xfs_da_format.h#L339-L345
type Dir2DataEntry struct {
	Inumber   uint64
	Namelen   uint8
	EntryName string
	Filetype  uint8
	Tag       uint16
}

// https://github.com/torvalds/linux/blob/5bfc75d92efd494db37f5c4c173d3639d4772966/fs/xfs/libxfs/xfs_da_format.h#L209-L220
type Dir2SfEntry struct {
	Namelen   uint8
	Offset    [2]uint8
	EntryName string
	Filetype  uint8
	Inumber   uint32
}

func (e Dir2SfEntry) String() string {
	return fmt.Sprintf("%20s (type: %d, inode: %d)", e.Name(), e.Filetype, e.Inumber)
}

func (e Dir2DataEntry) String() string {
	return fmt.Sprintf("%20s (type: %d, inode: %d tag: %x)", e.Name(), e.Filetype, e.Inumber, e.Tag)
}

type Device struct{}

type SymlinkString struct {
	Name string
}

type InodeCore struct {
	Magic        [2]byte
	Mode         uint16
	Version      uint8
	Format       uint8
	OnLink       uint16
	UID          uint32
	GID          uint32
	NLink        uint32
	ProjId       uint16
	Padding      [8]byte
	Flushiter    uint16
	Atime        uint64
	Mtime        uint64
	Ctime        uint64
	Size         uint64
	Nblocks      uint64
	Extsize      uint32
	Nextents     uint32
	Anextents    uint16
	Forkoff      uint8
	Aformat      uint8
	Dmevmask     uint32
	Dmstate      uint16
	Flags        uint16
	Gen          uint32
	NextUnlinked uint32

	CRC         uint32
	Changecount uint64
	Lsn         uint64
	Flags2      uint64
	Cowextsize  uint32
	Padding2    [12]byte
	Crtime      uint64
	Ino         uint64
	MetaUUID    [16]byte
}

func (ic InodeCore) IsDir() bool {
	return ic.Mode&0x4000 != 0
}

func (ic InodeCore) IsRegular() bool {
	return ic.Mode&0x8000 != 0
}

func (ic InodeCore) IsSymlink() bool {
	return ic.Mode&0xA000 != 0
}

func (ic InodeCore) isSupported() bool {
	if ic.Version == 3 {
		return true
	}
	return false
}

type InobtRec struct {
	Startino  uint32
	Freecount uint32
	Free      uint64
}
