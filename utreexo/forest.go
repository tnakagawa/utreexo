package utreexo

import (
	"fmt"
	"os"
	"time"
)

// A FullForest is the entire accumulator of the UTXO set. This is
// what the bridge node stores.  Everything is always full.

/*
The forest is structured in the space of a tree numbered from the bottom left,
taking up the space of a perfect tree that can contain the whole forest.
This means that in most cases there will be null nodes in the tree.
That's OK; it helps reduce renumbering nodes and makes it easier to think about
addressing.  It also might work well for on-disk serialization.
There might be a better / optimal way to do this but it seems OK for now.
*/

// Forest :
type Forest struct {
	numLeaves uint64 // number of leaves in the forest (bottom row)

	// height of the forest.  NON INTUITIVE!
	// When there is only 1 tree in the forest, it is equal to the height of
	// that tree (2**h nodes).  If there are multiple trees, height will
	// be 1 higher than the highest tree in the forest.
	// While you could just run treeHeight(numLeaves), and pollard does just this,
	// here it incurs the cost of a reMap when you cross a power of 2 boundary.
	// So right now it reMaps on the way up, but NOT on the way down, so the
	// height can sometimes be higher than it would be as treeHeight(numLeaves)
	// A little weird; could remove this, but likely would have a performance
	// penalty if the set dances right above / below a power of 2 leaves.
	height uint8

	// "data" (not the best name but) is an interface to storing the forest
	// hashes.  There's ram based and disk based for now, maybe if one
	// is clearly better can go back to non-interface.
	data ForestData
	// moving to slice based forest.  more efficient, can be moved to
	// an on-disk file more easily (the subtree stuff should be changed
	// at that point to do runs of i/o).  Not sure about "deleting" as it
	// might not be needed at all with a slice.

	positionMap map[MiniHash]uint64 // map from hashes to positions.
	// Inverse of forestMap for leaves.

	// -------------------- following are just for testing / benchmarking
	// how many hashes this forest has computed
	HistoricHashes uint64

	// time taken in Remove() function
	TimeRem time.Duration
	// of which time in the moveSubTree() function
	TimeMST time.Duration

	// time taken in hash operations (reHash)
	TimeInHash time.Duration

	// time taken in Prove operations
	TimeInProve time.Duration

	// the time taken in verify operations
	TimeInVerify time.Duration
}

// NewForest : use ram if not given a file
func NewForest(forestFile *os.File) *Forest {
	f := new(Forest)
	f.numLeaves = 0
	f.height = 0

	if forestFile == nil {
		// for in-ram
		f.data = new(ramForestData)
	} else {
		// for on-disk
		d := new(diskForestData)
		d.f = forestFile
		f.data = d
	}

	f.data.resize(1)
	f.positionMap = make(map[MiniHash]uint64)
	return f
}

const sibSwap = false
const bridgeVerbose = false

// empty is needed for detection (to find errors) but I'm not sure it's needed
// for deletion.  I think you can just leave garbage around, as it'll either
// get immediately overwritten, or it'll be out to the right, beyond the edge
// of the forest
var empty [32]byte

// Remove :
func (f *Forest) Remove(dels []uint64) error {

	err := f.removev3(dels)
	if err != nil {
		return err
	}

	return nil
}

// removev3 uses top down swaps and hopefully works the exact same as before
// top down swaps are better suited to undoing deletions
func (f *Forest) removev3(dels []uint64) error {

	if uint64(len(dels)) > f.numLeaves {
		return fmt.Errorf("%d deletions but forest has %d leaves",
			len(dels), f.numLeaves)
	}
	nextNumLeaves := f.numLeaves - uint64(len(dels))

	// check that all dels are there
	for _, dpos := range dels {
		if dpos > f.numLeaves {
			return fmt.Errorf(
				"Trying to delete leaf at %d, beyond max %d", dpos, f.numLeaves)
		}
	}

	var dirt []uint64

	// fmt.Printf("v3 topDownTransform %d %d %d\n", dels, f.numLeaves, f.height)
	swaps := floorTransform(dels, f.numLeaves, f.height)
	// TODO really really shouldn't use floor transform here.
	// In fact I'm not sure floor transform should even exist.

	// TODO definitely not how to do this, way inefficient
	// don't even use dirt, do it like in pollard
	for _, s := range swaps {
		f.data.swapHash(s.from, s.to)
		if s.to < nextNumLeaves {
			// from as well?
			dirt = append(dirt, s.to)
			if s.from < nextNumLeaves {
				dirt = append(dirt, s.from)
			}
		}
		// OK well while we're using floortransform, EVERY swap is at
		// height 0 so just change position map here...
		f.positionMap[f.data.read(s.to).Mini()] = s.to
		f.positionMap[f.data.read(s.from).Mini()] = s.from
	}

	f.numLeaves = nextNumLeaves

	return f.reHash(dirt)
}

// reHash hashes new data in the forest based on dirty positions.
// right now it seems "dirty" means the node itself moved, not that the
// parent has changed children.
// TODO: switch the meaning of "dirt" to mean parents with changed children;
// this will probably make it a lot simpler.
func (f *Forest) reHash(dirt []uint64) error {
	if f.height == 0 || len(dirt) == 0 { // nothing to hash
		return nil
	}
	tops, topheights := getTopsReverse(f.numLeaves, f.height)
	// fmt.Printf("nl %d f.h %d tops %v\n", f.numLeaves, f.height, tops)

	dirty2d := make([][]uint64, f.height)
	h := uint8(0)
	dirtyRemaining := 0
	for _, pos := range dirt {
		if pos > f.numLeaves {
			return fmt.Errorf("Dirt %d exceeds numleaves %d", pos, f.numLeaves)
		}
		dHeight := detectHeight(pos, f.height)
		// increase height if needed
		for h < dHeight {
			h++
		}
		if h > f.height {
			return fmt.Errorf("postion %d at height %d but forest only %d high",
				pos, h, f.height)
		}
		// if bridgeVerbose {
		// fmt.Printf("h %d\n", h)
		// }
		dirty2d[h] = append(dirty2d[h], pos)
		dirtyRemaining++
	}

	// this is basically the same as VerifyBlockProof.  Could maybe split
	// it to a separate function to reduce redundant code..?
	// nah but pretty different beacuse the dirtyMap has stuff that appears
	// halfway up...

	var currentRow, nextRow []uint64

	// floor by floor
	for h = uint8(0); h < f.height; h++ {
		if bridgeVerbose {
			fmt.Printf("dirty %v\ncurrentRow %v\n", dirty2d[h], currentRow)
		}

		// merge nextRow and the dirtySlice.  They're both sorted so this
		// should be quick.  Seems like a CS class kindof algo but who knows.
		// Should be O(n) anyway.

		currentRow = mergeSortedSlices(currentRow, dirty2d[h])
		dirtyRemaining -= len(dirty2d[h])
		if dirtyRemaining == 0 && len(currentRow) == 0 {
			// done hashing early
			break
		}

		for i, pos := range currentRow {
			// skip if next is sibling
			if i+1 < len(currentRow) && currentRow[i]|1 == currentRow[i+1] {
				continue
			}
			if len(tops) == 0 {
				return fmt.Errorf(
					"currentRow %v no tops remaining, this shouldn't happen",
					currentRow)
			}
			// also skip if this is a top
			if pos == tops[0] {
				continue
			}

			right := pos | 1
			left := right ^ 1
			parpos := up1(left, f.height)

			//				fmt.Printf("bridge hash %d %04x, %d %04x -> %d\n",
			//					left, leftHash[:4], right, rightHash[:4], parpos)
			if f.data.read(left) == empty || f.data.read(right) == empty {
				f.data.write(parpos, empty)
			} else {
				par := Parent(f.data.read(left), f.data.read(right))
				f.HistoricHashes++
				f.data.write(parpos, par)
			}
			nextRow = append(nextRow, parpos)
		}
		if topheights[0] == h {
			tops = tops[1:]
			topheights = topheights[1:]
		}
		currentRow = nextRow
		nextRow = []uint64{}
	}

	return nil
}

// cleanup removes extraneous hashes from the forest.  Currently only the bottom
func (f *Forest) cleanup(overshoot uint64) {
	for p := f.numLeaves; p < f.numLeaves+overshoot; p++ {
		delete(f.positionMap, f.data.read(p).Mini()) // clear position map
		// TODO ^^^^ that probably does nothing
		f.data.write(p, empty) // clear forest
	}
}

// Add adds leaves to the forest.  This is the easy part.
func (f *Forest) Add(adds []LeafTXO) {
	f.addv2(adds)
}

// Add adds leaves to the forest.  This is the easy part.
func (f *Forest) addv2(adds []LeafTXO) {

	for _, add := range adds {
		// fmt.Printf("adding %x pos %d\n", add.Hash[:4], f.numLeaves)
		f.positionMap[add.Mini()] = f.numLeaves

		tops, _ := getTopsReverse(f.numLeaves, f.height)
		pos := f.numLeaves
		n := add.Hash
		f.data.write(pos, n)
		for h := uint8(0); (f.numLeaves>>h)&1 == 1; h++ {
			// grab, pop, swap, hash, new
			top := f.data.read(tops[h]) // grab
			//			fmt.Printf("grabbed %x from %d\n", top[:12], tops[h])
			n = Parent(top, n)       // hash
			pos = up1(pos, f.height) // rise
			f.data.write(pos, n)     // write
			//			fmt.Printf("wrote %x to %d\n", n[:4], pos)
		}
		f.numLeaves++
	}
	return
}

// Modify changes the forest, adding and deleting leaves and updating internal nodes.
// Note that this does not modify in place!  All deletes occur simultaneous with
// adds, which show up on the right.
// Also, the deletes need there to be correct proof data, so you should first call Verify().
func (f *Forest) Modify(adds []LeafTXO, dels []uint64) (*undoBlock, error) {
	numdels, numadds := uint64(len(dels)), uint64(len(adds))
	delta := numadds - numdels // watch 32/64 bit
	// remap to expand the forest if needed
	for f.numLeaves+delta > 1<<f.height {
		// fmt.Printf("current cap %d need %d\n",
		// 1<<f.height, f.numLeaves+delta)
		err := f.reMap(f.height + 1)
		if err != nil {
			return nil, err
		}
	}

	// v3 should do the exact same thing as v2 now
	err := f.removev3(dels)
	if err != nil {
		return nil, err
	}
	f.cleanup(numdels)

	// save the leaves past the edge for undo
	// dels hasn't been mangled by remove up above, right?
	// BuildUndoData takes all the stuff swapped to the right by removev3
	// and saves it in the order it's in, which should make it go back to
	// the right place when it's swapped in reverse
	ub := f.BuildUndoData(numadds, dels)

	f.addv2(adds)

	// fmt.Printf("done modifying block, added %d\n", len(adds))
	// fmt.Printf("post add %s\n", f.ToString())
	// for m, p := range f.positionMap {
	// 	fmt.Printf("%x @%d\t", m[:4], p)
	// }
	// fmt.Printf("\n")

	return ub, err
}

// reMap changes the height of the forest
func (f *Forest) reMap(destHeight uint8) error {

	if destHeight == f.height {
		return fmt.Errorf("can't remap %d to %d... it's the same",
			destHeight, destHeight)
	}

	if destHeight > f.height+1 || (f.height > 0 && destHeight < f.height-1) {
		return fmt.Errorf("changing by more than 1 height not programmed yet")
	}

	fmt.Printf("remap forest height %d -> %d\n", f.height, destHeight)

	// for height reduction
	if destHeight < f.height {
		return fmt.Errorf("height reduction not implemented")
	}
	// I don't think you ever need to remap down.  It really doesn't
	// matter.  Something to program someday if you feel like it for fun.

	// height increase
	f.data.resize(1 << destHeight)

	pos := uint64(1 << destHeight) // leftmost position of row 1
	reach := pos >> 1              // how much to next row up
	// start on row 1, row 0 doesn't move
	for h := uint8(1); h < destHeight; h++ {
		runLength := reach >> 1
		for x := uint64(0); x < runLength; x++ {
			// ok if source position is non-empty
			ok := f.data.size() > (pos>>1)+x &&
				f.data.read((pos>>1)+x) != empty
			src := f.data.read((pos >> 1) + x)
			if ok {
				f.data.write(pos+x, src)
				// f.forest[pos+x] = src
			}
		}
		pos += reach
		reach >>= 1
	}

	// zero out (what is now the) right half of the bottom row
	//	copy(t.fs[1<<(t.height-1):1<<t.height], make([]Hash, 1<<(t.height-1)))
	for x := uint64(1 << f.height); x < 1<<destHeight; x++ {
		// here you may actually need / want to delete?  but numleaves
		// should still ensure that you're not reading over the edge...
		f.data.write(x, empty)
	}

	f.height = destHeight
	return nil
}

// sanity checks forest sanity: does numleaves make sense, and are the tops
// populated?
func (f *Forest) sanity() error {

	if f.numLeaves > 1<<f.height {
		return fmt.Errorf("forest has %d leaves but insufficient height %d",
			f.numLeaves, f.height)
	}
	tops, _ := getTopsReverse(f.numLeaves, f.height)
	for _, t := range tops {
		if f.data.read(t) == empty {
			return fmt.Errorf("Forest has %d leaves %d tops, but top @%d is empty",
				f.numLeaves, len(tops), t)
		}
	}
	if uint64(len(f.positionMap)) > f.numLeaves {
		return fmt.Errorf("sanity: positionMap %d leaves but forest %d leaves",
			len(f.positionMap), f.numLeaves)
	}

	return nil
}

// PosMapSanity is costly / slow: check that everything in posMap is correct
func (f *Forest) PosMapSanity() error {
	for i := uint64(0); i < f.numLeaves; i++ {
		if f.positionMap[f.data.read(i).Mini()] != i {
			return fmt.Errorf("positionMap error: map says %x @%d but @%d",
				f.data.read(i).Prefix(), f.positionMap[f.data.read(i).Mini()], i)
		}
	}
	return nil
}

// RestoreForest restores the forest on restart. Needed when resuming after exiting.
// miscForestFile is where numLeaves and height is stored
func (f *Forest) RestoreForest(miscForestFile *os.File, forestFile *os.File) error {

	// This restores the numLeaves
	var byteLeaves [8]byte
	_, err := miscForestFile.Read(byteLeaves[:])
	if err != nil {
		return err
	}
	f.numLeaves = BtU64(byteLeaves[:])
	fmt.Println("Forest leaves:", f.numLeaves)

	// This restores the positionMap
	var i uint64
	for i = uint64(0); i < f.numLeaves; i++ {
		f.positionMap[f.data.read(i).Mini()] = i
	}
	if f.positionMap == nil {
		return fmt.Errorf("Generated positionMap is nil")
	}
	var s string
	for m, pos := range f.positionMap {
		s += fmt.Sprintf("pos %d, leaf %x\n", pos, m)
	}
	lol, err := os.OpenFile("generatedpositionmap", os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		panic(err)
	}
	_, err = lol.WriteString(s)
	if err != nil {
		panic(err)
	}

	// This restores the height
	var byteHeight [1]byte
	_, err = miscForestFile.Read(byteHeight[:])
	if err != nil {
		return err
	}
	f.height = BtU8(byteHeight[:])
	fmt.Println("Forest height:", f.height)

	return nil
}

func (f *Forest) PrintPositionMap(file *os.File) {
	var s string
	for m, pos := range f.positionMap {
		s += fmt.Sprintf("pos %d, leaf %x\n", pos, m)
	}
	_, err := file.WriteString(s)
	if err != nil {
		panic(err)
	}
}

// WriteForest writes the numLeaves and height to miscForestFile
func (f *Forest) WriteForest(miscForestFile *os.File) error {
	fmt.Println("numLeaves=", f.numLeaves)
	fmt.Println("f.height=", f.height)
	_, err := miscForestFile.WriteAt(append(U64tB(f.numLeaves), U8tB(f.height)...), 0)
	if err != nil {
		return err
	}
	return nil
}

// GetTops returns all the tops of the trees
func (f *Forest) GetTops() []Hash {

	topposs, _ := getTopsReverse(f.numLeaves, f.height)
	tops := make([]Hash, len(topposs))

	for i := range tops {
		tops[i] = f.data.read(topposs[i])
	}

	return tops
}

// Stats :
func (f *Forest) Stats() string {

	s := fmt.Sprintf("numleaves: %d hashesever: %d posmap: %d forest: %d\n",
		f.numLeaves, f.HistoricHashes, len(f.positionMap), f.data.size())

	s += fmt.Sprintf("\thashT: %.2f remT: %.2f (of which MST %.2f) proveT: %.2f",
		f.TimeInHash.Seconds(), f.TimeRem.Seconds(), f.TimeMST.Seconds(),
		f.TimeInProve.Seconds())
	return s
}

// ToString prints out the whole thing.  Only viable for small forests
func (f *Forest) ToString() string {

	fh := f.height
	// tree height should be 6 or less
	if fh > 6 {
		return "forest too big to print "
	}

	output := make([]string, (fh*2)+1)
	var pos uint8
	for h := uint8(0); h <= fh; h++ {
		rowlen := uint8(1 << (fh - h))

		for j := uint8(0); j < rowlen; j++ {
			var valstring string
			ok := f.data.size() >= uint64(pos)
			if ok {
				val := f.data.read(uint64(pos))
				if val != empty {
					valstring = fmt.Sprintf("%x", val[:2])
				}
			}
			if valstring != "" {
				output[h*2] += fmt.Sprintf("%02d:%s ", pos, valstring)
			} else {
				output[h*2] += fmt.Sprintf("        ")
			}
			if h > 0 {
				//				if x%2 == 0 {
				output[(h*2)-1] += "|-------"
				for q := uint8(0); q < ((1<<h)-1)/2; q++ {
					output[(h*2)-1] += "--------"
				}
				output[(h*2)-1] += "\\       "
				for q := uint8(0); q < ((1<<h)-1)/2; q++ {
					output[(h*2)-1] += "        "
				}

				//				}

				for q := uint8(0); q < (1<<h)-1; q++ {
					output[h*2] += "        "
				}

			}
			pos++
		}

	}
	var s string
	for z := len(output) - 1; z >= 0; z-- {
		s += output[z] + "\n"
	}
	return s
}
