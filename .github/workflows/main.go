// You can edit this code!
// Click here and start typing.
package main

import (
	"fmt"
	"time"
)

func main() {
	var (
		tickerCount int
		loopCount   int
	)

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop() // please add this to prevent memory leak - https://stackoverflow.com/questions/68289916/will-time-tick-cause-memory-leak-when-i-never-need-to-stop-it

	done := make(chan struct{})
	loopCh := make(chan struct{})

	go func() {
		for range ticker.C {
			fmt.Println("! ticker", tickerCount)
			tickerCount++

			select {
			case <-done:
			default:
				close(done) // it is not correct for multiple goroutines, only for example
			}
		}
	}()

	time.Sleep(4 * time.Second)
	close(loopCh)
	<-done

	fmt.Println("DONE", tickerCount, loopCount)
}
