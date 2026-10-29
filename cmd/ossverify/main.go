// ossverify 是 OSS（S3 兼容接入点）的验收工具，不参与服务运行。
//
// 它用与应用完全相同的 minio-go 版本，逐条验证 RAM 策略授权范围内的动作：
// ListObjects、PutObject、StatObject、GetObject、
// 分片上传的 NewMultipartUpload/PutObjectPart/ListObjectParts/Abort、
// 以及预签名 GET/PUT 的 URL 生成。口令只从环境变量读，不打印。
//
// HeadBucket 只打印结果、不计入失败：阿里云 OSS 的 S3 兼容 HeadBucket
// 即使授予 oss:GetBucketInfo 也返回 403，应用启动路径已经改用 ListObjects
// 探测桶（见 internal/platform/objectstore.ensureBucket）。
//
// 用法见 docs/ACK_MANAGED_RESOURCES.md 第 4.5 节。
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var fail int

func pass(format string, a ...any) { fmt.Printf("[PASS] "+format+"\n", a...) }
func bad(format string, a ...any)  { fail++; fmt.Printf("[FAIL] "+format+"\n", a...) }

func main() {
	endpoint := os.Getenv("OSS_ENDPOINT")
	bucket := os.Getenv("OSS_BUCKET")
	ak, sk := os.Getenv("OSS_AK"), os.Getenv("OSS_SK")
	lookup := os.Getenv("OSS_LOOKUP")
	if endpoint == "" || bucket == "" || ak == "" || sk == "" {
		fmt.Fprintln(os.Stderr, "需要 OSS_ENDPOINT/OSS_BUCKET/OSS_AK/OSS_SK")
		os.Exit(2)
	}
	if lookup == "" {
		lookup = "auto"
	}

	opts := &minio.Options{
		Creds:  credentials.NewStaticV4(ak, sk, ""),
		Secure: true,
	}
	if lookup == "dns" {
		opts.BucketLookup = minio.BucketLookupDNS
	}
	client, err := minio.New(endpoint, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "初始化客户端失败: %v\n", err)
		os.Exit(2)
	}

	fmt.Printf("endpoint=%s bucket=%s lookup=%s\n", endpoint, bucket, lookup)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// 诊断开关：账户策略若允许 ListBuckets，直接列出真实桶名，
	// 免得在"桶名是否属于本账号"上反复猜。
	if os.Getenv("OSS_LIST_BUCKETS") == "1" {
		if bks, lerr := client.ListBuckets(ctx); lerr != nil {
			fmt.Printf("[INFO] ListBuckets 不可用: %s\n", describe(lerr))
		} else {
			for _, b := range bks {
				fmt.Printf("[INFO] bucket=%s created=%s\n", b.Name, b.CreationDate.Format(time.RFC3339))
			}
		}
	}

	// 1. 列举（ListObjects）——只依赖 oss:ListObjects，先跑它可以把
	// "策略没授 ListObjects" 与 "HeadBucket 缺 GetBucketInfo" 区分开。
	n := 0
	listErr := error(nil)
	for obj := range client.ListObjects(ctx, bucket, minio.ListObjectsOptions{MaxKeys: 5, Recursive: true}) {
		if obj.Err != nil {
			listErr = obj.Err
			break
		}
		n++
	}
	if listErr != nil {
		bad("ListObjects: %s", describe(listErr))
	} else {
		pass("ListObjects（前 5 个对象，实际返回 %d）", n)
	}

	// 2. 桶可达性：应用启动用 ListObjects 探测，上面的列举已经覆盖；
	// 这里再打一次 HeadBucket 仅作参考，403 不判失败（OSS 兼容行为）。
	if exists, err := client.BucketExists(ctx, bucket); err != nil {
		fmt.Printf("[INFO] HeadBucket 不可用（应用不依赖）: %s\n", describe(err))
	} else if !exists {
		fmt.Println("[INFO] HeadBucket 返回不存在（以 ListObjects 结果为准）")
	} else {
		fmt.Println("[INFO] HeadBucket 可用")
	}

	key := fmt.Sprintf("acceptance/%d.txt", time.Now().UnixNano())
	body := []byte("expensio oss acceptance\n")

	// 3. 上传（PutObject）
	if _, err := client.PutObject(ctx, bucket, key, bytes.NewReader(body), int64(len(body)),
		minio.PutObjectOptions{ContentType: "text/plain"}); err != nil {
		bad("PutObject: %v", err)
	} else {
		pass("PutObject %s", key)
	}

	// 4. 元数据（StatObject）
	if st, err := client.StatObject(ctx, bucket, key, minio.StatObjectOptions{}); err != nil {
		bad("StatObject: %v", err)
	} else if st.Size != int64(len(body)) {
		bad("StatObject 大小不符: got %d want %d", st.Size, len(body))
	} else {
		pass("StatObject size=%d contentType=%s", st.Size, st.ContentType)
	}

	// 5. 读回（GetObject）
	if obj, err := client.GetObject(ctx, bucket, key, minio.GetObjectOptions{}); err != nil {
		bad("GetObject: %v", err)
	} else {
		got, rerr := io.ReadAll(obj)
		_ = obj.Close()
		if rerr != nil {
			bad("GetObject 读取: %v", rerr)
		} else if !bytes.Equal(got, body) {
			bad("GetObject 内容不一致")
		} else {
			pass("GetObject 内容一致")
		}
	}

	// 6. 分片上传四步（策略里单独列了 ListParts / AbortMultipartUpload）
	core := minio.Core{Client: client}
	mpKey := key + ".multipart"
	uploadID, err := core.NewMultipartUpload(ctx, bucket, mpKey, minio.PutObjectOptions{})
	if err != nil {
		bad("NewMultipartUpload: %v", err)
	} else {
		pass("NewMultipartUpload uploadID 已签发")
		partBody := bytes.Repeat([]byte("a"), 5*1024*1024)
		part, perr := core.PutObjectPart(ctx, bucket, mpKey, uploadID, 1,
			bytes.NewReader(partBody), int64(len(partBody)), minio.PutObjectPartOptions{})
		if perr != nil {
			// OSS 对 aws-chunked 流式签名的支持与 AWS 不同，退一步用 unsigned payload。
			part, perr = core.PutObjectPart(ctx, bucket, mpKey, uploadID, 1,
				bytes.NewReader(partBody), int64(len(partBody)),
				minio.PutObjectPartOptions{DisableContentSha256: true})
		}
		if perr != nil {
			bad("PutObjectPart: %v", perr)
		} else {
			pass("PutObjectPart part=%d etag=%s", part.PartNumber, part.ETag)
		}
		if res, lerr := core.ListObjectParts(ctx, bucket, mpKey, uploadID, 0, 10); lerr != nil {
			bad("ListObjectParts: %v", lerr)
		} else {
			pass("ListObjectParts 已上传分片数=%d", len(res.ObjectParts))
		}
		if aerr := core.AbortMultipartUpload(ctx, bucket, mpKey, uploadID); aerr != nil {
			bad("AbortMultipartUpload: %v", aerr)
		} else {
			pass("AbortMultipartUpload")
		}
	}

	// 7. 预签名 URL（只写文件，不打印含签名的 URL）
	dir := os.Getenv("OSS_PRESIGN_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	getURL, err := client.PresignedGetObject(ctx, bucket, key, 10*time.Minute, nil)
	if err != nil {
		bad("PresignedGetObject: %v", err)
	} else {
		_ = os.WriteFile(filepath.Join(dir, "oss-presign-get.txt"), []byte(getURL.String()), 0o600)
		pass("PresignedGetObject（URL 写入 %s）", filepath.Join(dir, "oss-presign-get.txt"))
	}
	putURL, err := client.PresignedPutObject(ctx, bucket, key+".presigned", 10*time.Minute)
	if err != nil {
		bad("PresignedPutObject: %v", err)
	} else {
		_ = os.WriteFile(filepath.Join(dir, "oss-presign-put.txt"), []byte(putURL.String()), 0o600)
		pass("PresignedPutObject（URL 写入 %s）", filepath.Join(dir, "oss-presign-put.txt"))
	}
	// 应用实际上传时用 PresignHeader 把 Content-Type 签进 URL（附件类型
	// 要参与校验），与上面不带头的 PresignedPutObject 是两条代码路径。
	ctURL, err := client.PresignHeader(ctx, http.MethodPut, bucket, key+".presigned-ct",
		10*time.Minute, nil, http.Header{"Content-Type": []string{"text/plain"}})
	if err != nil {
		bad("PresignHeader(PUT, Content-Type): %v", err)
	} else {
		_ = os.WriteFile(filepath.Join(dir, "oss-presign-put-ct.txt"), []byte(ctURL.String()), 0o600)
		pass("PresignHeader(PUT, Content-Type)（URL 写入 %s）", filepath.Join(dir, "oss-presign-put-ct.txt"))
	}

	// 8. 删除（DeleteObject）
	if err := client.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{}); err != nil {
		bad("RemoveObject: %v", err)
	} else {
		pass("RemoveObject %s", key)
	}
	// 预签名 URL 由 curl 在外部写入对象，这里顺手清掉，避免桶里越积越多。
	// 对象可能不存在（外部没跑 curl），失败不影响结论。
	for _, k := range []string{key + ".presigned", key + ".presigned-ct"} {
		_ = client.RemoveObject(ctx, bucket, k, minio.RemoveObjectOptions{})
	}

	printVerdict()
}

func printVerdict() {
	if fail == 0 {
		fmt.Println("OSS_VERIFY: PASS")
		return
	}
	fmt.Println("OSS_VERIFY: FAIL")
	os.Exit(1)
}

// describe 把 minio 的错误响应展开成 OSS 返回码，权限问题与网络问题才能区分。
func describe(err error) string {
	er := minio.ToErrorResponse(err)
	if er.Code != "" {
		return fmt.Sprintf("code=%s message=%s requestID=%s", er.Code, er.Message, er.RequestID)
	}
	return err.Error()
}
