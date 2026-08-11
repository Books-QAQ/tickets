package util

import (
	"fmt"
	"math/rand"
	"strings"
	"time"
)

const alphabet = "abcdefghijklmnopqrstuvwxyz"

func init() {
	rand.Seed(time.Now().UnixNano())
}

// RandomInt generates a random integer between min and max
func RandomInt(min, max int64) int64 { //这个函数的作用是生成一个在 min 和 max 之间的随机整数，包含 min 和 max 本身
	return min + rand.Int63n(max-min+1)
}

// RandomString generates a random string of length n
func RandomString(n int) string { //这个函数的作用是生成一个长度为 n 的随机字符串，字符串由小写字母组成
	var sb strings.Builder
	k := len(alphabet)

	for i := 0; i < n; i++ {
		c := alphabet[rand.Intn(k)]
		sb.WriteByte(c)
	}

	return sb.String()
}

// RandomEmail generates a random email
func RandomEmail() string { //这个函数的作用是生成一个随机的电子邮件地址，格式为 <随机字符串>@email.com
	return fmt.Sprintf("%s@email.com", RandomString(6))
}

// RandomUsername generates a random owner name
func RandomUsername() string { //这个函数的作用是生成一个随机的用户名，长度为 6 个字符，由小写字母组成
	return RandomString(6)
}
