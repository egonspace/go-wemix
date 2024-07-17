package eth

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus/wpoa"
	"github.com/ethereum/go-ethereum/rpc"
	"math/big"
)

type PublicWemixAPI struct {
	e *Ethereum
}

type BriocheConfigResult struct {
	BlockReward       *hexutil.Big   `json:"blockReward,omitempty"`
	FirstHalvingBlock *hexutil.Big   `json:"firstHalvingBlock,omitempty"`
	HalvingPeriod     *hexutil.Big   `json:"halvingPeriod,omitempty"`
	FinishRewardBlock *hexutil.Big   `json:"finishRewardBlock,omitempty"`
	HalvingTimes      hexutil.Uint64 `json:"halvingTimes,omitempty"`
	HalvingRate       hexutil.Uint64 `json:"halvingRate,omitempty"`
}

type HalvingInfo struct {
	HalvingTimes hexutil.Uint64 `json:"halvingTimes"`
	StartBlock   *hexutil.Big   `json:"startBlock"`
	EndBlock     *hexutil.Big   `json:"endBlock"`
	BlockReward  *hexutil.Big   `json:"blockReward"`
}

func NewPublicWemixAPI(e *Ethereum) *PublicWemixAPI {
	return &PublicWemixAPI{e}
}

func (api *PublicWemixAPI) BriocheConfig() BriocheConfigResult {
	bc := api.e.BlockChain().Config().Brioche
	return BriocheConfigResult{
		BlockReward:       (*hexutil.Big)(bc.BlockReward),
		FirstHalvingBlock: (*hexutil.Big)(bc.FirstHalvingBlock),
		HalvingPeriod:     (*hexutil.Big)(bc.HalvingPeriod),
		FinishRewardBlock: (*hexutil.Big)(bc.FinishRewardBlock),
		HalvingTimes:      hexutil.Uint64(bc.HalvingTimes),
		HalvingRate:       hexutil.Uint64(bc.HalvingRate),
	}
}

func (api *PublicWemixAPI) HalvingSchedule() []*HalvingInfo {
	bc := api.e.BlockChain().Config().Brioche
	if bc.FirstHalvingBlock == nil || bc.HalvingPeriod == nil || bc.HalvingTimes == 0 {
		return nil
	}

	var lastRewardBlock *big.Int
	if bc.FinishRewardBlock != nil {
		lastRewardBlock = new(big.Int).Sub(bc.FinishRewardBlock, common.Big1)
	}

	result := make([]*HalvingInfo, 0)
	for i := uint64(0); i < bc.HalvingTimes; i++ {
		startBlock := new(big.Int).Add(bc.FirstHalvingBlock, new(big.Int).Mul(bc.HalvingPeriod, new(big.Int).SetUint64(i)))
		if lastRewardBlock != nil && startBlock.Cmp(lastRewardBlock) > 0 {
			break
		}
		result = append(result, &HalvingInfo{
			HalvingTimes: hexutil.Uint64(i + 1),
			StartBlock:   (*hexutil.Big)(startBlock),
			EndBlock:     (*hexutil.Big)(new(big.Int).Sub(new(big.Int).Add(startBlock, bc.HalvingPeriod), common.Big1)),
			BlockReward:  (*hexutil.Big)(api.getBriocheBlockReward(startBlock)),
		})
	}

	result[len(result)-1].EndBlock = (*hexutil.Big)(lastRewardBlock)

	return result
}

func (api *PublicWemixAPI) GetBriocheBlockReward(blockNumber rpc.BlockNumber) *hexutil.Big {
	height := new(big.Int)
	switch blockNumber {
	case rpc.LatestBlockNumber:
		height.Set(api.e.BlockChain().CurrentHeader().Number)
	case rpc.FinalizedBlockNumber:
		height.Set(api.e.BlockChain().CurrentHeader().Number)
	case rpc.PendingBlockNumber:
		block, _, _ := api.e.miner.Pending()
		height.Set(block.Header().Number)
	default:
		height.SetInt64(blockNumber.Int64())
	}

	return (*hexutil.Big)(api.getBriocheBlockReward(height))
}

func (api *PublicWemixAPI) getBriocheBlockReward(blockNumber *big.Int) *big.Int {
	info, err := wpoa.Info(api.e.engine)
	if err != nil {
		return nil
	}

	config := api.e.BlockChain().Config()
	height := new(big.Int).Set(blockNumber)

	if config.IsBrioche(height) {
		return config.Brioche.GetBriocheBlockReward(info.DefaultBriocheBlockReward, height)
	} else {
		return info.BlockReward
	}
}
