# PersonalDrive
Drive on top of likes of S3 or equivalent to give cheaper storage cost 


The cost of storing **100 GB of data in Amazon S3** depends on the **storage class** you choose. Below is a cost breakdown for **standard S3 pricing in the US East (N. Virginia)** region as of June 2025:

---

### 💾 **S3 Storage Classes (Monthly Cost for 100 GB)**

| Storage Class                 | Cost per GB/month | 100 GB Monthly Cost | Use Case                                   |
| ----------------------------- | ----------------- | ------------------- | ------------------------------------------ |
| S3 Standard                   | \$0.023           | \$2.30              | Frequent access (default, general purpose) |
| S3 Intelligent-Tiering        | \$0.023           | \$2.30 + monitoring | Automatic tiering                          |
| S3 Standard-IA (Infrequent)   | \$0.0125          | \$1.25              | Less frequent access (min 30-day storage)  |
| S3 One Zone-IA                | \$0.01            | \$1.00              | Infrequent access, single AZ               |
| S3 Glacier Instant Retrieval  | \$0.004           | \$0.40              | Archive, fast restore                      |
| S3 Glacier Flexible Retrieval | \$0.0036          | \$0.36              | Archive, slower restore                    |
| S3 Glacier Deep Archive       | \$0.00099         | \$0.099             | Long-term cold storage                     |

> ✅ **Note:**

* **Additional costs** may apply for PUT, GET, lifecycle transitions, and retrievals (especially for archival classes).
* **Monitoring and automation fee** for S3 Intelligent-Tiering is \$0.0025 per 1,000 objects.

---

### 📌 Example:

If you're storing **100 GB in S3 Standard**, expect to pay:

```
100 GB * $0.023/GB = $2.30/month
```
