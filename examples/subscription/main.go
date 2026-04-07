package main

import (
	"fmt"
	"sync"
	"time"

	tags "github.com/apiarytech/honeycomb"
	plc "github.com/apiarytech/royaljelly"
)

func main() {
	fmt.Println("--- Honeycomb Tag Subscription Example ---")

	// 1. Initialize the database and add a tag to monitor.
	db := tags.NewTagDatabase()
	tagName := "MyCounter"
	db.AddTag(&tags.Tag{Name: tagName, TypeInfo: &tags.TypeInfo{DataType: tags.TypeDINT}, Value: plc.DINT(0)})
	fmt.Printf("Added tag '%s' with initial value 0.\n", tagName)

	// 2. Subscribe to the tag.
	// This returns a read-only channel (`<-chan tags.Tag`) that will receive a copy
	// of the tag's state whenever its value changes. It also returns a unique
	// subscription ID, which is needed to unsubscribe later.
	subChan, subID, err := db.SubscribeToTag(tagName)
	if err != nil {
		panic(err)
	}
	fmt.Printf("Successfully subscribed to '%s' with subscription ID %d.\n\n", tagName, subID)

	// Use a WaitGroup to keep the main function alive long enough to see the final output.
	var wg sync.WaitGroup
	wg.Add(1)

	// 3. Start a separate goroutine to listen for notifications.
	// This is a common pattern for event-driven programming. The main application
	// can continue its work while this goroutine waits for and reacts to changes.
	go func() {
		defer wg.Done()
		// Ranging over the channel is a clean way to process updates.
		// The loop will automatically exit when the channel is closed by UnsubscribeFromTag.
		for updatedTag := range subChan {
			fmt.Printf("--> Notification Received: Tag '%s' changed to %v\n", updatedTag.Name, updatedTag.Value)
		}
		fmt.Println("--> Subscription channel has been closed.")
	}()

	// 4. Modify the tag's value multiple times.
	// Each call to SetTagValue will trigger a notification to be sent to the channel.
	fmt.Println("Setting tag value to 10...")
	db.SetTagValue(tagName, plc.DINT(10))
	time.Sleep(50 * time.Millisecond) // Small delay to make output readable

	fmt.Println("Setting tag value to 20...")
	db.SetTagValue(tagName, plc.DINT(20))
	time.Sleep(50 * time.Millisecond)

	// 5. Unsubscribe from the tag.
	// This stops notifications for this subscriber and closes the associated channel.
	fmt.Printf("\nUnsubscribing with ID %d...\n", subID)
	db.UnsubscribeFromTag(tagName, subID)

	// Wait for the subscriber goroutine to finish.
	wg.Wait()
	fmt.Println("\n--- Example Finished ---")
}
