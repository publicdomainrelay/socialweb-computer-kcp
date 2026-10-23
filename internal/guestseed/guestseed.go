package guestseed

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

const Label = "cidata"

const (
	sectorBytes   = 512
	rootEntries   = 512
	minClusters   = 4096
	maxClusters   = 65524
	maxPerCluster = 64
	fatDate       = 0x0021
)

type Seed struct {
	UserData      []byte
	MetaData      []byte
	NetworkConfig []byte
}

type document struct {
	name  string
	short string
	body  []byte
	first int
}

func (s Seed) documents() ([]*document, error) {
	if len(s.UserData) == 0 {
		return nil, fmt.Errorf("guestseed: the seed has no user-data, and user-data is the whole " +
			"contract a requester asked for: a guest booted from a seed without it comes up with " +
			"nothing installed and nothing reporting")
	}
	if len(s.MetaData) == 0 {
		return nil, fmt.Errorf("guestseed: the seed has no meta-data; NoCloud refuses a seed " +
			"without an instance-id, so the guest would run its datasource search to None instead")
	}
	docs := []*document{
		{name: "meta-data", short: "META-D~1   ", body: s.MetaData},
		{name: "user-data", short: "USER-D~1   ", body: s.UserData},
	}
	if len(s.NetworkConfig) > 0 {
		docs = append(docs, &document{name: "network-config", short: "NETWOR~1   ", body: s.NetworkConfig})
	}
	return docs, nil
}

func Image(seed Seed) ([]byte, error) {
	docs, err := seed.documents()
	if err != nil {
		return nil, err
	}
	perCluster := 1
	var clusters int
	for {
		need := 0
		for _, d := range docs {
			need += (len(d.body) + perCluster*sectorBytes - 1) / (perCluster * sectorBytes)
		}
		clusters = max(need+8, minClusters)
		if clusters <= maxClusters {
			break
		}
		perCluster *= 2
		if perCluster > maxPerCluster {
			return nil, fmt.Errorf("guestseed: the documents total more than a FAT16 seed holds")
		}
	}
	fatSectors := ((clusters+2)*2 + sectorBytes - 1) / sectorBytes
	rootSectors := rootEntries * 32 / sectorBytes
	total := 1 + 2*fatSectors + rootSectors + clusters*perCluster
	img := make([]byte, total*sectorBytes)

	sum := sha256.New()
	for _, d := range docs {
		fmt.Fprintf(sum, "%s\x00%d\x00", d.name, len(d.body))
		sum.Write(d.body)
	}
	serial := sum.Sum(nil)[:4]

	b := img[:sectorBytes]
	copy(b[0:3], []byte{0xEB, 0x3C, 0x90})
	copy(b[3:11], "SWCSEED ")
	binary.LittleEndian.PutUint16(b[11:], sectorBytes)
	b[13] = byte(perCluster)
	binary.LittleEndian.PutUint16(b[14:], 1)
	b[16] = 2
	binary.LittleEndian.PutUint16(b[17:], rootEntries)
	if total < 0x10000 {
		binary.LittleEndian.PutUint16(b[19:], uint16(total))
	} else {
		binary.LittleEndian.PutUint32(b[32:], uint32(total))
	}
	b[21] = 0xF8
	binary.LittleEndian.PutUint16(b[22:], uint16(fatSectors))
	binary.LittleEndian.PutUint16(b[24:], 32)
	binary.LittleEndian.PutUint16(b[26:], 64)
	b[36] = 0x80
	b[38] = 0x29
	copy(b[39:43], serial)
	copy(b[43:54], padded(Label, 11))
	copy(b[54:62], "FAT16   ")
	b[510], b[511] = 0x55, 0xAA

	fat := make([]byte, fatSectors*sectorBytes)
	binary.LittleEndian.PutUint16(fat[0:], 0xFFF8)
	binary.LittleEndian.PutUint16(fat[2:], 0xFFFF)
	dataStart := (1 + 2*fatSectors + rootSectors) * sectorBytes
	clusterBytes := perCluster * sectorBytes
	next := 2
	for _, d := range docs {
		n := (len(d.body) + clusterBytes - 1) / clusterBytes
		d.first = next
		for i := range n {
			c := next + i
			link := uint16(c + 1)
			if i == n-1 {
				link = 0xFFFF
			}
			binary.LittleEndian.PutUint16(fat[2*c:], link)
		}
		copy(img[dataStart+(next-2)*clusterBytes:], d.body)
		next += n
	}
	for i := range 2 {
		copy(img[(1+i*fatSectors)*sectorBytes:], fat)
	}

	root := img[(1+2*fatSectors)*sectorBytes : dataStart]
	at := 0
	label := root[0:32]
	copy(label[0:11], padded(Label, 11))
	label[11] = 0x08
	binary.LittleEndian.PutUint16(label[24:], fatDate)
	at += 32
	for _, d := range docs {
		for _, e := range longNameEntries(d.name, d.short) {
			copy(root[at:at+32], e)
			at += 32
		}
		e := root[at : at+32]
		copy(e[0:11], d.short)
		e[11] = 0x20
		binary.LittleEndian.PutUint16(e[16:], fatDate)
		binary.LittleEndian.PutUint16(e[18:], fatDate)
		binary.LittleEndian.PutUint16(e[24:], fatDate)
		binary.LittleEndian.PutUint16(e[26:], uint16(d.first))
		binary.LittleEndian.PutUint32(e[28:], uint32(len(d.body)))
		at += 32
	}
	return img, nil
}

func padded(s string, n int) string {
	return s + strings.Repeat(" ", n-len(s))
}

func shortChecksum(short string) byte {
	var sum byte
	for i := range 11 {
		sum = (sum&1)<<7 + sum>>1 + short[i]
	}
	return sum
}

func longNameEntries(name, short string) [][]byte {
	units := utf16.Encode([]rune(name))
	count := (len(units) + 12) / 13
	slots := make([]uint16, count*13)
	for i := range slots {
		switch {
		case i < len(units):
			slots[i] = units[i]
		case i == len(units):
			slots[i] = 0
		default:
			slots[i] = 0xFFFF
		}
	}
	check := shortChecksum(short)
	out := make([][]byte, 0, count)
	for seq := count; seq >= 1; seq-- {
		e := make([]byte, 32)
		e[0] = byte(seq)
		if seq == count {
			e[0] |= 0x40
		}
		e[11] = 0x0F
		e[13] = check
		part := slots[(seq-1)*13 : seq*13]
		k := 0
		for _, r := range [][2]int{{1, 11}, {14, 26}, {28, 32}} {
			for o := r[0]; o < r[1]; o += 2 {
				binary.LittleEndian.PutUint16(e[o:], part[k])
				k++
			}
		}
		out = append(out, e)
	}
	return out
}

func WriteFile(path string, seed Seed) error {
	img, err := Image(seed)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cidata-")
	if err != nil {
		return fmt.Errorf("guestseed: %w", err)
	}
	if _, err := tmp.Write(img); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("guestseed: writing %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("guestseed: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o444); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("guestseed: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("guestseed: placing the seed at %s: %w", path, err)
	}
	return nil
}
