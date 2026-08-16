package main

import (
	"fmt"

	"github.com/mrvladis/photosync/internal/gphotos"
	"github.com/mrvladis/photosync/internal/inventory"
	"github.com/mrvladis/photosync/internal/journal"
	"github.com/mrvladis/photosync/internal/transfer"
)

func newRunner(j *journal.Journal, client *gphotos.Client, c config,
	workers, limit int, dryRun, describe, convertRAW bool,
	floorGB, dailyBudget int64, quality int, exts []string) *transfer.Runner {
	return transfer.New(j, client, transfer.Options{
		SourceRoot:         inventory.OneDriveMount() + "/" + c.source,
		Workers:            workers,
		Limit:              limit,
		Extensions:         exts,
		DryRun:             dryRun,
		DescribeWithPath:   describe,
		ConvertRAW:         convertRAW,
		ConvertQuality:     quality,
		FreeSpaceFloor:     floorGB << 30,
		DailyRequestBudget: dailyBudget,
		Progress:           func(s string) { fmt.Println("  " + s) },
	})
}
