package model

import "time"

// FitnessScore はタスク実行結果の機械的評価スコアを表す。
// 辞書式順序で比較され、候補の勝敗判定に使用される。
type FitnessScore struct {
	Passed           bool
	RepairCount      int
	DiffFilesChanged int
	DiffLinesChanged int
	PathDeviation    bool // expected_paths からの逸脱有無
	ExecutionTime    time.Duration
}

// FitnessThresholds は Fitness 比較時のマージン閾値を定義する。
// 各軸の差がマージン以内であれば同等と見なす。
type FitnessThresholds struct {
	RepairCountMargin   int
	DiffSizeMargin      int
	ExecutionTimeMargin time.Duration
}

// DefaultFitnessThresholds はデフォルトの閾値を返す。
func DefaultFitnessThresholds() FitnessThresholds {
	return FitnessThresholds{
		RepairCountMargin:   0,
		DiffSizeMargin:      10,
		ExecutionTimeMargin: 30 * time.Second,
	}
}

// IsFailed は Passed が false かどうかを返す。
func (a FitnessScore) IsFailed() bool {
	return !a.Passed
}

// Compare は a と b を辞書式4軸で比較する。
// a が勝ちなら -1、b が勝ちなら +1、同等なら 0 を返す。
func (a FitnessScore) Compare(b FitnessScore, th FitnessThresholds) int {
	// 軸1: Pass / Fail
	if a.Passed != b.Passed {
		if a.Passed {
			return -1
		}
		return 1
	}

	// 軸2: RepairCount（少ない方が勝ち）
	repairDiff := a.RepairCount - b.RepairCount
	if abs(repairDiff) > th.RepairCountMargin {
		if repairDiff < 0 {
			return -1
		}
		return 1
	}

	// 軸3: PathDeviation（逸脱なしが勝ち）
	if a.PathDeviation != b.PathDeviation {
		if !a.PathDeviation {
			return -1
		}
		return 1
	}

	// 軸4: DiffLinesChanged（少ない方が勝ち）
	diffLinesDiff := a.DiffLinesChanged - b.DiffLinesChanged
	if abs(diffLinesDiff) > th.DiffSizeMargin {
		if diffLinesDiff < 0 {
			return -1
		}
		return 1
	}

	// 軸5: ExecutionTime（短い方が勝ち）
	timeDiff := a.ExecutionTime - b.ExecutionTime
	if timeDiff < 0 {
		timeDiff = -timeDiff
	}
	if timeDiff > th.ExecutionTimeMargin {
		if a.ExecutionTime < b.ExecutionTime {
			return -1
		}
		return 1
	}

	return 0
}

// SelectWinner は候補群から勝者インデックスを返す。
// 全候補が同等の場合は isTie=true, winnerIndex=0 を返す。
// 空の候補群の場合は winnerIndex=-1, isTie=false を返す。
func SelectWinner(candidates []FitnessScore, th FitnessThresholds) (winnerIndex int, isTie bool) {
	if len(candidates) == 0 {
		return -1, false
	}
	if len(candidates) == 1 {
		return 0, false
	}

	best := 0
	allTie := true

	for i := 1; i < len(candidates); i++ {
		cmp := candidates[best].Compare(candidates[i], th)
		if cmp > 0 {
			// candidates[i] が勝ち
			best = i
			allTie = false
		} else if cmp < 0 {
			// candidates[best] が勝ち
			allTie = false
		}
	}

	if allTie && len(candidates) > 1 {
		return 0, true
	}

	// best が本当に全候補に対して勝ちまたは同等かを再検証
	for i := 0; i < len(candidates); i++ {
		if i == best {
			continue
		}
		cmp := candidates[best].Compare(candidates[i], th)
		if cmp > 0 {
			// best より強い候補がある → トーナメント結果と矛盾
			// 推移律が成立するため本来起こらないが安全策
			return 0, true
		}
	}

	return best, allTie
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
