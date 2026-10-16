package main

import "fmt"

func quick_sort(arr []int, low, high int) {
	if low < high {
		pivot_index := partition(arr, low, high)
		quick_sort(arr, low, pivot_index-1)
		quick_sort(arr, pivot_index+1, high)
	}
}

func partition(arr []int, low, high int) int {
	pivot := arr[high]
	i := low - 1
	for j := low; j < high; j++ {
		if arr[j] <= pivot {
			i++
			arr[i], arr[j] = arr[j], arr[i]
		}
	}
	arr[i+1], arr[high] = arr[high], arr[i+1]
	return i + 1
}

func main() {
	arr := []int{10, 7, 8, 9, 1, 5}
	fmt.Println("排序前:", arr)
	quick_sort(arr, 0, len(arr)-1)
	fmt.Println("排序后:", arr)
}
