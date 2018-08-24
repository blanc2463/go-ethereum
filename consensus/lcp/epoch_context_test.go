package lcp

import (
	"errors"
	"fmt"
	"math/big"
	"testing"

	"github.com/pavelkrolevets/go-ethereum/common"
	"github.com/pavelkrolevets/go-ethereum/core/state"
	"github.com/pavelkrolevets/go-ethereum/core/types"
	"github.com/pavelkrolevets/go-ethereum/ethdb"
	"github.com/stretchr/testify/assert"

	"fmt"
	"github.com/pavelkrolevets/go-ethereum/trie"
	"strconv"
	"strings"
)

func TestEpochContextCountVotes(t *testing.T) {
	voteMap := map[common.Address][]common.Address{
		common.HexToAddress("0x44d1ce0b7cb3588bca96151fe1bc05af38f91b6e"): {
			common.HexToAddress("0xb040353ec0f2c113d5639444f7253681aecda1f8"),
		},
		common.HexToAddress("0xa60a3886b552ff9992cfcd208ec1152079e046c2"): {
			common.HexToAddress("0x14432e15f21237013017fa6ee90fc99433dec82c"),
			common.HexToAddress("0x9f30d0e5c9c88cade54cd1adecf6bc2c7e0e5af6"),
		},
		common.HexToAddress("0x4e080e49f62694554871e669aeb4ebe17c4a9670"): {
			common.HexToAddress("0xd83b44a3719720ec54cdb9f54c0202de68f1ebcb"),
			common.HexToAddress("0x56cc452e450551b7b9cffe25084a069e8c1e9441"),
			common.HexToAddress("0xbcfcb3fa8250be4f2bf2b1e70e1da500c668377b"),
		},
		common.HexToAddress("0x9d9667c71bb09d6ca7c3ed12bfe5e7be24e2ffe1"): {},
	}
	balance := int64(5)
	db := ethdb.NewMemDatabase()
	//dbd := trie.NewDatabase(db)
	stateDB, _ := state.New(common.Hash{}, state.NewDatabase(db))
	LCPContext, err := types.NewLCPContext(db)
	assert.Nil(t, err)

	epochContext := &EpochContext{
		Context: LCPContext,
		statedb: stateDB,
	}

	// _, err = epochContext.countVotes()
	_, err = epochContext.cVotes()
	assert.NotNil(t, err)

	for candidate, electors := range voteMap {
		assert.Nil(t, LCPContext.BecomeCandidate(candidate))
		//for _, delegate := range electors{
		//	assert.Nil(t, LCPContext.BecomeDelegate(delegate))
		//	fmt.Println(delegate)
		//}
		//fmt.Println(electors)
		for _, elector := range electors {
			epochContext.statedb.SetBalance(elector, big.NewInt(balance))
			//bal := epochContext.statedb.GetBalance(elector)
			//fmt.Println(bal)
			assert.Nil(t, LCPContext.Delegate(elector, candidate))
		}
	}

	result, err := epochContext.countVotes()
	//fmt.Println(result)
	assert.Nil(t, err)
	assert.Equal(t, len(voteMap), len(result))

	for candidate, electors := range voteMap {
		voteCount, ok := result[candidate]
		assert.True(t, ok)

		fmt.Println("voteCount =", voteCount)
		fmt.Println("ele =", balance*int64(len(electors)))

		assert.Equal(t, balance*int64(len(electors)), voteCount.Int64())
	}
}

func TestLookupValidator(t *testing.T) {
	db := ethdb.NewMemDatabase()
	dposCtx, _ := types.NewLCPContext(db)
	mockEpochContext := &EpochContext{
		Context: dposCtx,
	}
	validators := []common.Address{
		common.StringToAddress("addr1"),
		common.StringToAddress("addr2"),
		common.StringToAddress("addr3"),
	}
	mockEpochContext.Context.SetPeriodBlock(1)
	mockEpochContext.Context.SetEpochInterval(86400)
	mockEpochContext.Context.SetMaxValidators(3)
	blockInterval := mockEpochContext.Context.GetPeriodBlock()
	maxval := mockEpochContext.Context.GetMaxValidators()
	epoch_int := mockEpochContext.Context.GetEpochInterval()

	fmt.Println(blockInterval, maxval, epoch_int)
	mockEpochContext.Context.SetValidators(validators)
	for i, expected := range validators {
		fmt.Println(i, expected)
		got, _ := mockEpochContext.lookupValidator(int64(i) * blockInterval)
		if got != expected {
			t.Errorf("Failed to test lookup validator, %s was expected but got %s", expected.Str(), got.Str())
		}
	}
	_, err := mockEpochContext.lookupValidator(blockInterval - 1)
	if err != ErrInvalidMintBlockTime {
		t.Errorf("Failed to test lookup validator. err '%v' was expected but got '%v'", ErrInvalidMintBlockTime, err)
	}
}

func TestEpochContextKickoutValidator(t *testing.T) {
	db := ethdb.NewMemDatabase()
	stateDB, _ := state.New(common.Hash{}, state.NewDatabase(db))
	dposContext, err := types.NewLCPContext(db)
	assert.Nil(t, err)

	epochContext := &EpochContext{
		TimeStamp: epochInterval,
		Context:   dposContext,
		statedb:   stateDB,
	}
	epochContext.Context.SetPeriodBlock(1)
	epochContext.Context.SetEpochInterval(86400)
	epochContext.Context.SetMaxValidators(3)

	maxValidatorSize := int(epochContext.Context.GetMaxValidators())
	atLeastMintCnt := epochInterval / blockInterval / int64(maxValidatorSize) / 2
	testEpoch := int64(1)

	// no validator can be kickout, because all validators mint enough block at least
	validators := []common.Address{}
	for i := 0; i < int(maxValidatorSize); i++ {
		validator := common.StringToAddress("addr" + strconv.Itoa(i))
		validators = append(validators, validator)
		assert.Nil(t, dposContext.BecomeCandidate(validator))
		setTestMintCnt(dposContext, testEpoch, validator, atLeastMintCnt)
	}
	assert.Nil(t, dposContext.SetValidators(validators))
	assert.Nil(t, dposContext.BecomeCandidate(common.StringToAddress("addr")))
	assert.Nil(t, epochContext.kickoutValidator(testEpoch))
	candidateMap := getCandidates(dposContext.CandidateTrie())
	assert.Equal(t, maxValidatorSize+1, len(candidateMap))

	// atLeast a safeSize count candidate will reserve
	dposContext, err = types.NewLCPContext(db)
	assert.Nil(t, err)
	epochContext = &EpochContext{
		TimeStamp: epochInterval,
		Context:   dposContext,
		statedb:   stateDB,
	}
	validators = []common.Address{}
	for i := 0; i < int(maxValidatorSize); i++ {
		validator := common.StringToAddress("addr" + strconv.Itoa(i))
		validators = append(validators, validator)
		assert.Nil(t, dposContext.BecomeCandidate(validator))
		setTestMintCnt(dposContext, testEpoch, validator, atLeastMintCnt-int64(i)-1)
	}
	assert.Nil(t, dposContext.SetValidators(validators))
	assert.Nil(t, epochContext.kickoutValidator(testEpoch))
	candidateMap = getCandidates(dposContext.CandidateTrie())
	assert.Equal(t, safeSize, len(candidateMap))
	for i := int(maxValidatorSize) - 1; i >= safeSize; i-- {
		assert.False(t, candidateMap[common.StringToAddress("addr"+strconv.Itoa(i))])
	}

	// all validator will be kickout, because all validators didn't mint enough block at least
	dposContext, err = types.NewLCPContext(db)
	assert.Nil(t, err)
	epochContext = &EpochContext{
		TimeStamp: epochInterval,
		Context:   dposContext,
		statedb:   stateDB,
	}
	validators = []common.Address{}
	for i := 0; i < int(maxValidatorSize); i++ {
		validator := common.StringToAddress("addr" + strconv.Itoa(i))
		validators = append(validators, validator)
		assert.Nil(t, dposContext.BecomeCandidate(validator))
		setTestMintCnt(dposContext, testEpoch, validator, atLeastMintCnt-1)
	}
	for i := int(maxValidatorSize); i < int(maxValidatorSize)*2; i++ {
		candidate := common.StringToAddress("addr" + strconv.Itoa(i))
		assert.Nil(t, dposContext.BecomeCandidate(candidate))
	}
	assert.Nil(t, dposContext.SetValidators(validators))
	assert.Nil(t, epochContext.kickoutValidator(testEpoch))
	candidateMap = getCandidates(dposContext.CandidateTrie())
	assert.Equal(t, maxValidatorSize, len(candidateMap))

	// only one validator mint count is not enough
	dposContext, err = types.NewLCPContext(db)
	assert.Nil(t, err)
	epochContext = &EpochContext{
		TimeStamp: epochInterval,
		Context:   dposContext,
		statedb:   stateDB,
	}
	validators = []common.Address{}
	for i := 0; i < int(maxValidatorSize); i++ {
		validator := common.StringToAddress("addr" + strconv.Itoa(i))
		validators = append(validators, validator)
		assert.Nil(t, dposContext.BecomeCandidate(validator))
		if i == 0 {
			setTestMintCnt(dposContext, testEpoch, validator, atLeastMintCnt-1)
		} else {
			setTestMintCnt(dposContext, testEpoch, validator, atLeastMintCnt)
		}
	}
	assert.Nil(t, dposContext.BecomeCandidate(common.StringToAddress("addr")))
	assert.Nil(t, dposContext.SetValidators(validators))
	assert.Nil(t, epochContext.kickoutValidator(testEpoch))
	candidateMap = getCandidates(dposContext.CandidateTrie())
	assert.Equal(t, maxValidatorSize, len(candidateMap))
	assert.False(t, candidateMap[common.StringToAddress("addr"+strconv.Itoa(0))])

	// epochTime is not complete, all validators mint enough block at least
	dposContext, err = types.NewLCPContext(db)
	assert.Nil(t, err)
	epochContext = &EpochContext{
		TimeStamp: epochInterval / 2,
		Context:   dposContext,
		statedb:   stateDB,
	}
	validators = []common.Address{}
	for i := 0; i < int(maxValidatorSize); i++ {
		validator := common.StringToAddress("addr" + strconv.Itoa(i))
		validators = append(validators, validator)
		assert.Nil(t, dposContext.BecomeCandidate(validator))
		setTestMintCnt(dposContext, testEpoch, validator, atLeastMintCnt/2)
	}
	for i := int(maxValidatorSize); i < int(maxValidatorSize)*2; i++ {
		candidate := common.StringToAddress("addr" + strconv.Itoa(i))
		assert.Nil(t, dposContext.BecomeCandidate(candidate))
	}
	assert.Nil(t, dposContext.SetValidators(validators))
	assert.Nil(t, epochContext.kickoutValidator(testEpoch))
	candidateMap = getCandidates(dposContext.CandidateTrie())
	assert.Equal(t, maxValidatorSize*2, len(candidateMap))

	// epochTime is not complete, all validators didn't mint enough block at least
	dposContext, err = types.NewLCPContext(db)
	assert.Nil(t, err)
	epochContext = &EpochContext{
		TimeStamp: epochInterval / 2,
		Context:   dposContext,
		statedb:   stateDB,
	}
	validators = []common.Address{}
	for i := 0; i < int(maxValidatorSize); i++ {
		validator := common.StringToAddress("addr" + strconv.Itoa(i))
		validators = append(validators, validator)
		assert.Nil(t, dposContext.BecomeCandidate(validator))
		setTestMintCnt(dposContext, testEpoch, validator, atLeastMintCnt/2-1)
	}
	for i := int(maxValidatorSize); i < int(maxValidatorSize)*2; i++ {
		candidate := common.StringToAddress("addr" + strconv.Itoa(i))
		assert.Nil(t, dposContext.BecomeCandidate(candidate))
	}
	assert.Nil(t, dposContext.SetValidators(validators))
	assert.Nil(t, epochContext.kickoutValidator(testEpoch))
	candidateMap = getCandidates(dposContext.CandidateTrie())
	assert.Equal(t, maxValidatorSize, len(candidateMap))

	dposContext, err = types.NewLCPContext(db)
	assert.Nil(t, err)
	epochContext = &EpochContext{
		TimeStamp: epochInterval / 2,
		Context:   dposContext,
		statedb:   stateDB,
	}
	assert.NotNil(t, epochContext.kickoutValidator(testEpoch))
	dposContext.SetValidators([]common.Address{})
	assert.NotNil(t, epochContext.kickoutValidator(testEpoch))
}

func setTestMintCnt(dposContext *types.LCPContext, epoch int64, validator common.Address, count int64) {
	for i := int64(0); i < count; i++ {
		updateMintCnt(epoch*epochInterval, epoch*epochInterval+blockInterval, validator, dposContext)
	}
}

func getCandidates(candidateTrie *trie.Trie) map[common.Address]bool {
	candidateMap := map[common.Address]bool{}
	iter := trie.NewIterator(candidateTrie.NodeIterator(nil))
	for iter.Next() {
		candidateMap[common.BytesToAddress(iter.Value)] = true
	}
	return candidateMap
}

func TestEpochContextTryElect(t *testing.T) {
	db := ethdb.NewMemDatabase()
	stateDB, _ := state.New(common.Hash{}, state.NewDatabase(db))
	dposContext, err := types.NewLCPContext(db)
	assert.Nil(t, err)
	epochContext := &EpochContext{
		TimeStamp: epochInterval,
		Context:   dposContext,
		statedb:   stateDB,
	}
	maxValidatorSize := int(epochContext.Context.GetMaxValidators())
	atLeastMintCnt := epochInterval / blockInterval / int64(maxValidatorSize) / 2
	testEpoch := int64(1)
	validators := []common.Address{}
	for i := 0; i < maxValidatorSize; i++ {
		validator := common.StringToAddress("addr" + strconv.Itoa(i))
		validators = append(validators, validator)
		assert.Nil(t, dposContext.BecomeCandidate(validator))
		assert.Nil(t, dposContext.Delegate(validator, validator))
		stateDB.SetBalance(validator, big.NewInt(1))
		setTestMintCnt(dposContext, testEpoch, validator, atLeastMintCnt-1)
	}
	dposContext.BecomeCandidate(common.StringToAddress("more"))
	assert.Nil(t, dposContext.SetValidators(validators))

	// genesisEpoch == parentEpoch do not kickout
	genesis := &types.Header{
		Time: big.NewInt(0),
	}
	parent := &types.Header{
		Time: big.NewInt(epochInterval - blockInterval),
	}
	oldHash := dposContext.EpochTrie().Hash()
	assert.Nil(t, epochContext.tryElect(genesis, parent))
	result, err := dposContext.GetValidators()
	assert.Nil(t, err)
	assert.Equal(t, maxValidatorSize, len(result))
	for _, validator := range result {
		assert.True(t, strings.Contains(validator.Str(), "addr"))
	}
	assert.NotEqual(t, oldHash, dposContext.EpochTrie().Hash())

	// genesisEpoch != parentEpoch and have none mintCnt do not kickout
	genesis = &types.Header{
		Time: big.NewInt(-epochInterval),
	}
	parent = &types.Header{
		Difficulty: big.NewInt(1),
		Time:       big.NewInt(epochInterval - blockInterval),
	}
	epochContext.TimeStamp = epochInterval
	oldHash = dposContext.EpochTrie().Hash()
	assert.Nil(t, epochContext.tryElect(genesis, parent))
	result, err = dposContext.GetValidators()
	assert.Nil(t, err)
	assert.Equal(t, maxValidatorSize, len(result))
	for _, validator := range result {
		assert.True(t, strings.Contains(validator.Str(), "addr"))
	}
	assert.NotEqual(t, oldHash, dposContext.EpochTrie().Hash())

	// genesisEpoch != parentEpoch kickout
	genesis = &types.Header{
		Time: big.NewInt(0),
	}
	parent = &types.Header{
		Time: big.NewInt(epochInterval*2 - blockInterval),
	}
	epochContext.TimeStamp = epochInterval * 2
	oldHash = dposContext.EpochTrie().Hash()
	assert.Nil(t, epochContext.tryElect(genesis, parent))
	result, err = dposContext.GetValidators()
	assert.Nil(t, err)
	assert.Equal(t, safeSize, len(result))
	moreCnt := 0
	for _, validator := range result {
		if strings.Contains(validator.Str(), "more") {
			moreCnt++
		}
	}
	assert.Equal(t, 1, moreCnt)
	assert.NotEqual(t, oldHash, dposContext.EpochTrie().Hash())

	// parentEpoch == currentEpoch do not elect
	genesis = &types.Header{
		Time: big.NewInt(0),
	}
	parent = &types.Header{
		Time: big.NewInt(epochInterval),
	}
	epochContext.TimeStamp = epochInterval + blockInterval
	oldHash = dposContext.EpochTrie().Hash()
	assert.Nil(t, epochContext.tryElect(genesis, parent))
	result, err = dposContext.GetValidators()
	assert.Nil(t, err)
	assert.Equal(t, safeSize, len(result))
	assert.Equal(t, oldHash, dposContext.EpochTrie().Hash())
}
